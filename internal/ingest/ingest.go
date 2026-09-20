// Package ingest streams both Amazon reports into the single ledger table,
// resolving the mapping configs and accumulating the accounting summary in the
// same pass (no post-ingest aggregation step).
package ingest

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/rahuldabola/portone-recon/internal/mapping"
	"github.com/rahuldabola/portone-recon/internal/model"
	"github.com/shopspring/decimal"
)

// Ingester holds the resolved config and the running summary.
type Ingester struct {
	PaymentRules    *mapping.Set
	SettlementRules *mapping.Set

	// ScopeSettlementIDs limits which settlement the report covers. It is
	// populated from the settlement file's header rows, so the payments side is
	// always scoped to the settlement we actually hold settlement data for.
	ScopeSettlementIDs map[string]bool

	// Summary is accumulated as rows stream past: Summary[source][field].
	Summary map[model.Source]map[string]*Bucket

	// Unmapped collects rows no config rule matched, so they can be reported
	// rather than silently contributing nothing.
	Unmapped map[string]*UnmappedGroup

	Settlements []model.Settlement
	Diagnostics []mapping.Diagnostic
}

// Bucket is one accumulated summary cell.
type Bucket struct {
	Amount decimal.Decimal
	Count  int
}

// UnmappedGroup aggregates rows that matched no rule.
type UnmappedGroup struct {
	Source            model.Source
	TransactionType   string
	AmountType        string
	Description       string
	AmountField       string
	Count             int
	Amount            decimal.Decimal
	FirstSourceLine   int
}

// New builds an Ingester from already-loaded config rules.
func New(paymentRules, settlementRules []mapping.Rule) *Ingester {
	ing := &Ingester{
		PaymentRules:       mapping.NewSet(paymentRules),
		SettlementRules:    mapping.NewSet(settlementRules),
		ScopeSettlementIDs: map[string]bool{},
		Summary: map[model.Source]map[string]*Bucket{
			model.SourcePayment:    {},
			model.SourceSettlement: {},
		},
		Unmapped: map[string]*UnmappedGroup{},
	}
	ing.Diagnostics = append(ing.Diagnostics, mapping.Lint("PAYMENT", paymentRules)...)
	ing.Diagnostics = append(ing.Diagnostics, mapping.Lint("SETTLEMENT", settlementRules)...)
	return ing
}

// resolve matches an entry against the config, builds its record_ref, routes it
// to a summary bucket by sign, and emits it.
//
// When more than one rule matches, every match is emitted: that is a config
// defect which double-counts the amount, and the pipeline reproduces it
// faithfully (route_seq > 0) instead of quietly choosing one.
func (ing *Ingester) resolve(e *model.LedgerEntry, rules *mapping.Set, q mapping.Query, emit func(model.LedgerEntry) error) error {
	matches := rules.Match(q)

	if len(matches) == 0 {
		// A payments row only populates a handful of its money columns. A zero
		// in an unmapped column is not a mapping defect, it is just an empty
		// cell, so only non-zero amounts are reported as unmapped.
		if !e.Amount.IsZero() {
			ing.noteUnmapped(e)
		}
		e.MatchKind = string(mapping.MatchUnmapped)
		e.InScope = ing.inScope(e)
		return emit(*e)
	}

	for i, r := range matches {
		out := *e
		out.RouteSeq = i
		id := r.ID
		out.ConfigID = &id
		out.MatchKind = string(mapping.KindOf(r, q, r.TransactionType == "" && q.TransactionType != ""))
		out.SummaryField = r.SummaryField(out.Amount)

		ref, _ := mapping.BuildRecordRef(r.Template, mapping.Fields{
			TxnRef:          out.TxnRef,
			SKU:             out.SKU,
			Date:            dateToken(out),
			SettlementID:    out.SettlementID,
			ShipmentID:      out.ShipmentID,
			MerchantOrderID: out.MerchantOrderID,
			AdjustmentID:    out.AdjustmentID,
			Description:     out.Description,
			RecordType:      out.TransactionType,
		})
		out.RecordRef = ref
		out.InScope = ing.inScope(&out)

		// Streaming summary: accumulate here, at ingest time.
		if out.InScope && out.SummaryField != "" && !out.Amount.IsZero() {
			ing.accumulate(out.Source, out.SummaryField, out.Amount)
		}
		if err := emit(out); err != nil {
			return err
		}
	}
	return nil
}

func dateToken(e model.LedgerEntry) string {
	if e.EventDate.IsZero() {
		return ""
	}
	return e.EventDate.UTC().Format("2006-01-02")
}

func (ing *Ingester) accumulate(src model.Source, field string, amt decimal.Decimal) {
	b := ing.Summary[src][field]
	if b == nil {
		b = &Bucket{}
		ing.Summary[src][field] = b
	}
	b.Amount = b.Amount.Add(amt)
	b.Count++
}

// inScope decides whether a row belongs to the settlement period under report.
//
//   - Settlement rows: in scope when they belong to a settlement we hold.
//   - Payments rows:   in scope when they carry that settlement id AND are
//     Released. Deferred rows are posted inside the period but explicitly not
//     released into this settlement — Amazon settles them in a later one — so
//     counting them would overstate this settlement.
func (ing *Ingester) inScope(e *model.LedgerEntry) bool {
	if len(ing.ScopeSettlementIDs) > 0 && !ing.ScopeSettlementIDs[e.SettlementID] {
		return false
	}
	if e.Source == model.SourcePayment {
		switch e.TransactionStatus {
		case "", "Released":
			return true
		default:
			return false
		}
	}
	return true
}

func (ing *Ingester) noteUnmapped(e *model.LedgerEntry) {
	k := fmt.Sprintf("%s|%s|%s|%s|%s", e.Source, e.TransactionType, e.AmountType, e.Description, e.AmountField)
	g := ing.Unmapped[k]
	if g == nil {
		g = &UnmappedGroup{
			Source: e.Source, TransactionType: e.TransactionType, AmountType: e.AmountType,
			Description: e.Description, AmountField: e.AmountField, FirstSourceLine: e.SourceLine,
		}
		ing.Unmapped[k] = g
	}
	g.Count++
	g.Amount = g.Amount.Add(e.Amount)
}

// UnmappedList returns unmapped groups ordered by absolute value, worst first.
func (ing *Ingester) UnmappedList() []*UnmappedGroup {
	out := make([]*UnmappedGroup, 0, len(ing.Unmapped))
	for _, g := range ing.Unmapped {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Amount.Abs().GreaterThan(out[j].Amount.Abs())
	})
	return out
}

// Run streams the settlement file first (to learn which settlement is under
// report) and then the payments file.
func (ing *Ingester) Run(paymentsPath, settlementsPath string, emit func(model.LedgerEntry) error) (payRows, setRows int, err error) {
	setRows, settles, err := ing.ReadSettlements(settlementsPath, emit)
	if err != nil {
		return 0, setRows, err
	}
	ing.Settlements = settles
	for _, s := range settles {
		ing.ScopeSettlementIDs[s.ID] = true
	}
	payRows, err = ing.ReadPayments(paymentsPath, emit)
	return payRows, setRows, err
}

// SHA256 returns the hex digest of a file, recorded on the ingest run so a
// re-ingest of identical inputs is recognisable.
func SHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, bufio.NewReaderSize(f, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// stripBOM transparently drops a leading UTF-8 byte order mark.
func stripBOM(r io.Reader) io.Reader {
	br := bufio.NewReaderSize(r, 1<<20)
	if b, err := br.Peek(3); err == nil && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		_, _ = br.Discard(3)
	}
	return br
}
