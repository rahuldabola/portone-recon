// Package recon matches the two sources on the shared record_ref.
//
// Granularity: the two files describe the same money at different grain — one
// payments row can face several settlement rows and vice versa (e.g. a single
// payments Order line faces ItemPrice/Principal + ItemFees/Commission +
// Promotion/Shipping rows; a single reimbursement order faces three settlement
// rows on the same day). So both sides are *aggregated to the record_ref first*
// and matched set-to-set, never row-to-row. record_ref is designed for exactly
// this: it is the coarsest key at which both reports agree.
package recon

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type Status string

const (
	Reconciled             Status = "Reconciled"
	UnreconciledPayment    Status = "Unreconciled - Payment only"
	UnreconciledSettlement Status = "Unreconciled - Settlement only"
)

// Side is one source's aggregate for a record_ref.
type Side struct {
	Rows        int   // ledger rows (amount components) behind this side
	SourceLines []int // file line numbers, for audit
	Total       decimal.Decimal
	Fields      map[string]decimal.Decimal
	Attrs       map[string]string // representative display attributes
	Raw         map[string]string // representative raw source row
}

func newSide() *Side {
	return &Side{Fields: map[string]decimal.Decimal{}, Attrs: map[string]string{}, Raw: map[string]string{}}
}

func (s *Side) Field(name string) decimal.Decimal {
	if s == nil {
		return decimal.Zero
	}
	return s.Fields[name]
}

// Pair is one reconciliation unit.
type Pair struct {
	RecordRef  string
	Payment    *Side
	Settlement *Side
	Status     Status
}

// Diff returns payment-minus-settlement for a summary field.
func (p Pair) Diff(field string) decimal.Decimal {
	return p.Payment.Field(field).Sub(p.Settlement.Field(field))
}

func (p Pair) TotalDiff() decimal.Decimal {
	var a, b decimal.Decimal
	if p.Payment != nil {
		a = p.Payment.Total
	}
	if p.Settlement != nil {
		b = p.Settlement.Total
	}
	return a.Sub(b)
}

// Result is the whole reconciliation.
type Result struct {
	Pairs                 []Pair
	CountReconciled       int
	CountUnrecPayment     int
	CountUnrecSettlement  int
	PaymentRowsInScope    int
	SettlementRowsInScope int
}

// Load runs the reconciliation against an ingest run.
func Load(ctx context.Context, pool *pgxpool.Pool, runID int64) (*Result, error) {
	sides := map[string]map[string]*Side{} // recordRef -> source -> side

	get := func(ref, src string) *Side {
		m := sides[ref]
		if m == nil {
			m = map[string]*Side{}
			sides[ref] = m
		}
		s := m[src]
		if s == nil {
			s = newSide()
			m[src] = s
		}
		return s
	}

	// 1. Money per (record_ref, source, summary_field).
	//    route_seq > 0 rows are the duplicate routings a defective config
	//    produces; they are included here deliberately, because they are what
	//    the (before-fix) Summary sheet actually reports.
	rows, err := pool.Query(ctx, `
		SELECT record_ref, source, summary_field, SUM(amount)::numeric
		  FROM ledger_entry
		 WHERE run_id = $1 AND in_scope AND record_ref <> '' AND summary_field <> ''
		 GROUP BY 1,2,3`, runID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ref, src, field string
		var amt decimal.Decimal
		if err := rows.Scan(&ref, &src, &field, &amt); err != nil {
			rows.Close()
			return nil, err
		}
		get(ref, src).Fields[field] = amt
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 2. Row totals per (record_ref, source).
	//    Payments: the report's own `total` column (amount_field = TOTAL).
	//    Settlement: the sum of its amount components — the file has no total
	//    column, and its components are the total by construction.
	//    route_seq = 0 keeps duplicate routings from inflating the total.
	rows, err = pool.Query(ctx, `
		SELECT record_ref, source,
		       SUM(CASE WHEN source = 'PAYMENT'
		                THEN CASE WHEN amount_field = 'TOTAL' THEN amount ELSE 0 END
		                ELSE amount END)::numeric AS total,
		       COUNT(*)::int AS rows,
		       (ARRAY_AGG(DISTINCT source_line ORDER BY source_line))[1:25] AS lines
		  FROM ledger_entry
		 WHERE run_id = $1 AND in_scope AND record_ref <> '' AND route_seq = 0
		 GROUP BY 1,2`, runID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ref, src string
		var total decimal.Decimal
		var n int
		var lines []int32
		if err := rows.Scan(&ref, &src, &total, &n, &lines); err != nil {
			rows.Close()
			return nil, err
		}
		s := get(ref, src)
		s.Total = total
		s.Rows = n
		for _, l := range lines {
			s.SourceLines = append(s.SourceLines, int(l))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 3. A representative source row per (record_ref, source) for the audit
	//    columns. The earliest line wins, so the choice is stable across runs.
	rows, err = pool.Query(ctx, `
		SELECT DISTINCT ON (record_ref, source)
		       record_ref, source, source_file, source_line, raw_payload,
		       settlement_id, txn_ref, sku, transaction_type_raw, description_raw,
		       marketplace, fulfilment, order_city, order_state, order_postal,
		       transaction_status, match_kind, summary_field,
		       to_char(event_date, 'YYYY-MM-DD'),
		       COALESCE(to_char(posted_at,   'YYYY-MM-DD HH24:MI:SS'), ''),
		       COALESCE(to_char(released_at, 'YYYY-MM-DD HH24:MI:SS'), ''),
		       COALESCE(quantity::text, '')
		  FROM ledger_entry
		 WHERE run_id = $1 AND in_scope AND record_ref <> '' AND route_seq = 0
		 ORDER BY record_ref, source, source_line`, runID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ref, src string
		var payload []byte
		var a struct {
			file, sku, txnRef, settlementID, typeRaw, descRaw    string
			marketplace, fulfilment, city, state, postal, status string
			matchKind, summaryField, eventDate, posted, released string
			qty                                                  string
		}
		var line int
		if err := rows.Scan(&ref, &src, &a.file, &line, &payload,
			&a.settlementID, &a.txnRef, &a.sku, &a.typeRaw, &a.descRaw,
			&a.marketplace, &a.fulfilment, &a.city, &a.state, &a.postal,
			&a.status, &a.matchKind, &a.summaryField,
			&a.eventDate, &a.posted, &a.released, &a.qty); err != nil {
			rows.Close()
			return nil, err
		}
		s := get(ref, src)
		s.Attrs = map[string]string{
			"source_file": a.file, "settlement_id": a.settlementID, "txn_ref": a.txnRef,
			"sku": a.sku, "type": a.typeRaw, "description": a.descRaw,
			"marketplace": a.marketplace, "fulfilment": a.fulfilment,
			"order_city": a.city, "order_state": a.state, "order_postal": a.postal,
			"status": a.status, "match_kind": a.matchKind, "summary_field": a.summaryField,
			"event_date": a.eventDate, "posted_at": a.posted, "released_at": a.released,
			"quantity": a.qty,
		}
		if len(payload) > 0 {
			raw := map[string]string{}
			if err := json.Unmarshal(payload, &raw); err == nil {
				s.Raw = raw
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 4. Classify and order: reconciled, then unreconciled payments, then
	//    unreconciled settlements (the order the assignment asks for).
	res := &Result{}
	refs := make([]string, 0, len(sides))
	for ref := range sides {
		refs = append(refs, ref)
	}
	sort.Strings(refs)

	var reconciled, unrecPay, unrecSet []Pair
	for _, ref := range refs {
		m := sides[ref]
		p := Pair{RecordRef: ref, Payment: m["PAYMENT"], Settlement: m["SETTLEMENT"]}
		switch {
		case p.Payment != nil && p.Settlement != nil:
			p.Status = Reconciled
			reconciled = append(reconciled, p)
		case p.Payment != nil:
			p.Status = UnreconciledPayment
			unrecPay = append(unrecPay, p)
		default:
			p.Status = UnreconciledSettlement
			unrecSet = append(unrecSet, p)
		}
	}
	res.CountReconciled = len(reconciled)
	res.CountUnrecPayment = len(unrecPay)
	res.CountUnrecSettlement = len(unrecSet)
	res.Pairs = append(append(reconciled, unrecPay...), unrecSet...)

	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE source='PAYMENT'),
		       COUNT(*) FILTER (WHERE source='SETTLEMENT')
		  FROM ledger_entry WHERE run_id=$1 AND in_scope AND record_ref <> '' AND route_seq=0`, runID).
		Scan(&res.PaymentRowsInScope, &res.SettlementRowsInScope); err != nil {
		return nil, err
	}
	return res, nil
}
