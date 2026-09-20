// Package mapping implements the config-driven half of the pipeline: matching a
// normalised source row against the marketplace mapping rules, building its
// record_ref, and choosing the summary bucket from the sign of the amount.
package mapping

import (
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
)

// Wildcard is the config's "matches any value for this column" marker. It is
// spelled "any" in the source CSVs; we compare in normalised (upper) form.
const Wildcard = "ANY"

// MatchKind records *how* a rule was selected, so the report can be audited.
type MatchKind string

const (
	MatchExact    MatchKind = "EXACT"    // transaction_type and description both matched literally
	MatchPrefix   MatchKind = "PREFIX"   // config description is a prefix of the row's description
	MatchWildcard MatchKind = "WILDCARD" // config description is `any`
	MatchFallback MatchKind = "FALLBACK" // matched a rule with an empty transaction_type
	MatchUnmapped MatchKind = "UNMAPPED" // no rule at all
)

// Rule is one row of either mapping config, in a shape common to both.
type Rule struct {
	ID         int
	SourceLine int

	// Payment configs key on (transaction_type, description, amount_field).
	// Settlement configs key on (transaction_type, amount_type, amount_description).
	TransactionType string
	Description     string // payments: description; settlement: amount_description
	AmountField     string // payments only
	AmountType      string // settlement only

	Template     string
	WhenPositive string
	WhenNegative string
}

// SummaryField picks the bucket for an amount. The config routes by sign; zero
// is treated as positive so that a 0.00 component still lands in a deterministic
// bucket (it contributes nothing either way, but keeps counts stable).
func (r Rule) SummaryField(amount decimal.Decimal) string {
	if amount.IsNegative() {
		return r.WhenNegative
	}
	return r.WhenPositive
}

// Set is a matchable collection of rules for one source.
type Set struct {
	rules []Rule
	// byType[transactionType] -> rules, including the "" fallback bucket.
	byType map[string][]Rule
}

// NewSet indexes rules for matching.
func NewSet(rules []Rule) *Set {
	s := &Set{rules: rules, byType: map[string][]Rule{}}
	for _, r := range rules {
		s.byType[r.TransactionType] = append(s.byType[r.TransactionType], r)
	}
	return s
}

func (s *Set) Rules() []Rule { return s.rules }

// Query is a normalised source row reduced to just its config key.
type Query struct {
	TransactionType string
	Description     string // payments description / settlement amount-description
	AmountField     string // payments only
	AmountType      string // settlement only
}

// Match returns every rule that matches the query, best first.
//
// Precedence, highest to lowest:
//  1. exact transaction_type + exact description
//  2. exact transaction_type + longest matching description prefix
//  3. exact transaction_type + `any` description
//
// The catch-all rules (those with an empty transaction_type) are reached only
// when the transaction type is *entirely unknown to the config* — not merely
// when this particular amount column has no rule under a known type.
//
// That distinction matters. The payments config carries catch-all rules keyed
// on amount_field (`,any,total`, `,any,other`, `,any,other_transaction_fees`)
// whose record_ref template is `txn_ref+settlement_id+date`. Falling back
// per-column would give every ORDER row a *second*, differently-shaped
// record_ref for its empty `other` column — 12,034 phantom "unreconciled
// payment" keys carrying no money. The catch-all is for a transaction type the
// config has never seen, so that is where it is applied.
//
// Normally exactly one rule comes back. More than one means the config routes a
// single amount into several buckets — a defect the caller surfaces rather than
// papers over. Zero means this amount column is not mapped for this type.
func (s *Set) Match(q Query) []Rule {
	if _, known := s.byType[q.TransactionType]; known {
		return s.matchWithin(s.byType[q.TransactionType], q, false)
	}
	if q.TransactionType != "" {
		return s.matchWithin(s.byType[""], q, true)
	}
	return nil
}

type scored struct {
	rule Rule
	tier int // 0 exact, 1 prefix, 2 wildcard
	plen int // prefix length, longer is better
	kind MatchKind
}

func (s *Set) matchWithin(candidates []Rule, q Query, fallback bool) []Rule {
	var out []scored
	for _, r := range candidates {
		// The amount_field / amount_type dimension is always an exact match:
		// it names which column of the source row the rule consumes.
		if r.AmountField != "" || q.AmountField != "" {
			if r.AmountField != q.AmountField {
				continue
			}
		}
		if r.AmountType != "" || q.AmountType != "" {
			if r.AmountType != q.AmountType && r.AmountType != Wildcard {
				continue
			}
		}

		sc := scored{rule: r}
		switch {
		case r.Description == q.Description && r.Description != Wildcard:
			sc.tier, sc.plen, sc.kind = 0, len(r.Description), MatchExact
		case r.Description == Wildcard:
			sc.tier, sc.kind = 2, MatchWildcard
		case r.Description != "" && strings.HasPrefix(q.Description, r.Description):
			// e.g. config "TO_ACCOUNT_ENDING" vs row "TO_ACCOUNT_ENDING_WITH:_334"
			sc.tier, sc.plen, sc.kind = 1, len(r.Description), MatchPrefix
		case r.Description == "" && q.Description == "":
			sc.tier, sc.kind = 0, MatchExact
		default:
			continue
		}
		if fallback {
			sc.kind = MatchFallback
		}
		out = append(out, sc)
	}
	if len(out) == 0 {
		return nil
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].tier != out[j].tier {
			return out[i].tier < out[j].tier
		}
		return out[i].plen > out[j].plen
	})
	// Keep only the rules in the winning tier: a wildcard rule must not fire
	// alongside the exact rule that beat it.
	best := out[0]
	var rules []Rule
	for _, sc := range out {
		if sc.tier != best.tier || sc.plen != best.plen {
			continue
		}
		r := sc.rule
		rules = append(rules, r)
	}
	return rules
}

// KindOf recomputes the match kind for a chosen rule against a query, for
// recording on the ledger row.
func KindOf(r Rule, q Query, fallback bool) MatchKind {
	switch {
	case fallback:
		return MatchFallback
	case r.Description == Wildcard:
		return MatchWildcard
	case r.Description == q.Description:
		return MatchExact
	case r.Description != "" && strings.HasPrefix(q.Description, r.Description):
		return MatchPrefix
	default:
		return MatchExact
	}
}

// ---------------------------------------------------------------------------
// record_ref
// ---------------------------------------------------------------------------

// Fields carries the values a record_ref template can interpolate.
type Fields struct {
	TxnRef          string
	SKU             string
	Date            string // YYYY-MM-DD, UTC
	SettlementID    string
	ShipmentID      string
	MerchantOrderID string
	AdjustmentID    string
	Description     string
	RecordType      string
}

// tokens are the template placeholders. They are lower-case in every config
// row, while literal segments are upper-case ("ADJUSTMENT", "LOST:WAREHOUSE",
// "GENERAL ADJUSTMENT", ...), so a case-sensitive lookup cleanly separates the
// two and a literal can never be mistaken for a field.
func (f Fields) token(name string) (string, bool) {
	switch name {
	case "txn_ref":
		return f.TxnRef, true
	case "sku":
		return f.SKU, true
	case "date":
		return f.Date, true
	case "settlement_id":
		return f.SettlementID, true
	case "shipment_id":
		return f.ShipmentID, true
	case "merchant_order_id":
		return f.MerchantOrderID, true
	case "adjustment_id":
		return f.AdjustmentID, true
	case "description":
		return f.Description, true
	case "record_type":
		return f.RecordType, true
	}
	return "", false
}

// BuildRecordRef renders a "+"-joined template against a row's fields.
//
// Literal segments pass through untouched. Field tokens are substituted. The
// result is upper-cased so that the payments side ("BIO-S000004059_AU") and the
// settlement side ("BIO-S000004059_AU") cannot drift apart on case alone.
func BuildRecordRef(template string, f Fields) (string, []string) {
	var (
		parts   []string
		missing []string
	)
	for _, seg := range strings.Split(template, "+") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if v, ok := f.token(seg); ok {
			if v == "" {
				missing = append(missing, seg)
			}
			parts = append(parts, v)
			continue
		}
		parts = append(parts, seg)
	}
	return strings.ToUpper(strings.Join(parts, "+")), missing
}

// Diagnostic reports a structural problem found in a config at load time.
type Diagnostic struct {
	Source string // PAYMENT / SETTLEMENT
	Kind   string
	Detail string
	Lines  []int
}

func (d Diagnostic) String() string {
	return fmt.Sprintf("[%s] %s: %s (config lines %v)", d.Source, d.Kind, d.Detail, d.Lines)
}

// Lint reports rules that share a key but route to different summary buckets,
// or that share a key but disagree on the record_ref template. Both are
// mapping defects: the first double-counts an amount, the second splits one
// logical record across two reconciliation keys.
func Lint(source string, rules []Rule) []Diagnostic {
	type key struct{ t, d, af, at string }
	byKey := map[key][]Rule{}
	for _, r := range rules {
		byKey[key{r.TransactionType, r.Description, r.AmountField, r.AmountType}] = append(
			byKey[key{r.TransactionType, r.Description, r.AmountField, r.AmountType}], r)
	}
	var out []Diagnostic
	for k, rs := range byKey {
		if len(rs) < 2 {
			continue
		}
		buckets := map[string]bool{}
		templates := map[string]bool{}
		var lines []int
		for _, r := range rs {
			buckets[r.WhenPositive+"/"+r.WhenNegative] = true
			templates[r.Template] = true
			lines = append(lines, r.SourceLine)
		}
		sort.Ints(lines)
		id := strings.Trim(strings.Join([]string{k.t, k.at, k.d, k.af}, "|"), "|")
		if len(buckets) > 1 {
			var bs []string
			for b := range buckets {
				bs = append(bs, b)
			}
			sort.Strings(bs)
			out = append(out, Diagnostic{source, "DUPLICATE_ROUTE",
				fmt.Sprintf("%s is routed into %d different buckets: %s", id, len(bs), strings.Join(bs, "  and  ")), lines})
		}
		if len(templates) > 1 {
			out = append(out, Diagnostic{source, "TEMPLATE_CONFLICT",
				fmt.Sprintf("%s has %d different record_ref templates", id, len(templates)), lines})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}
