package mapping

import (
	"testing"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal {
	v, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return v
}

// ---------------------------------------------------------------------------
// record_ref templating
// ---------------------------------------------------------------------------

func TestBuildRecordRef(t *testing.T) {
	f := Fields{
		TxnRef:       "503-8856864-4518217",
		SKU:          "BIO-S000004059_AU",
		Date:         "2026-07-17",
		SettlementID: "12395580393",
		ShipmentID:   "U2w85ZpVN",
		Description:  "TO_ACCOUNT_ENDING_WITH:_334",
	}
	cases := map[string]string{
		"txn_ref+sku+date":                      "503-8856864-4518217+BIO-S000004059_AU+2026-07-17",
		"txn_ref+settlement_id+date":            "503-8856864-4518217+12395580393+2026-07-17",
		"TRANSFER+description+settlement_id+date": "TRANSFER+TO_ACCOUNT_ENDING_WITH:_334+12395580393+2026-07-17",
		// literal segments must survive untouched, including ones with spaces,
		// colons and hyphens
		"txn_ref+sku+ADJUSTMENT+settlement_id+date": "503-8856864-4518217+BIO-S000004059_AU+ADJUSTMENT+12395580393+2026-07-17",
		"GENERAL ADJUSTMENT+sku+settlement_id+date": "GENERAL ADJUSTMENT+BIO-S000004059_AU+12395580393+2026-07-17",
		"DAMAGED:WAREHOUSE+sku+settlement_id+date":  "DAMAGED:WAREHOUSE+BIO-S000004059_AU+12395580393+2026-07-17",
		// note the shipment id is upper-cased, like every other segment
		"shipment_id+INBOUND_DEFECT_FEE+settlement_id+date": "U2W85ZPVN+INBOUND_DEFECT_FEE+12395580393+2026-07-17",
	}
	for tmpl, want := range cases {
		got, _ := BuildRecordRef(tmpl, f)
		if got != want {
			t.Errorf("BuildRecordRef(%q):\n got %q\nwant %q", tmpl, got, want)
		}
	}
}

// A literal token must never be mistaken for a field. Field tokens are
// lower-case in every config row; literals are upper-case.
func TestBuildRecordRefLiteralsAreNotFields(t *testing.T) {
	got, _ := BuildRecordRef("ADJUSTMENT_OTHER+settlement_id+date", Fields{
		SettlementID: "12395580393", Date: "2026-07-17",
		// deliberately set fields whose names collide in upper case
		TxnRef: "SHOULD-NOT-APPEAR",
	})
	want := "ADJUSTMENT_OTHER+12395580393+2026-07-17"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A missing field value must be reported, not silently produce a key that
// happens to collide with another record.
func TestBuildRecordRefReportsMissingFields(t *testing.T) {
	_, missing := BuildRecordRef("txn_ref+sku+date", Fields{SKU: "X", Date: "2026-07-17"})
	if len(missing) != 1 || missing[0] != "txn_ref" {
		t.Errorf("expected txn_ref to be reported missing, got %v", missing)
	}
}

func TestBuildRecordRefUpperCases(t *testing.T) {
	got, _ := BuildRecordRef("txn_ref+sku+date", Fields{
		TxnRef: "m0jcPjF2nw", SKU: "bio-collagenmask_au", Date: "2026-07-18",
	})
	if got != "M0JCPJF2NW+BIO-COLLAGENMASK_AU+2026-07-18" {
		t.Errorf("record_ref should be case-normalised, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// rule matching
// ---------------------------------------------------------------------------

func paymentRules() *Set {
	return NewSet([]Rule{
		{ID: 1, TransactionType: "ORDER", Description: "ANY", AmountField: "PRODUCT_SALES",
			Template: "txn_ref+sku+date", WhenPositive: "sales_product_charges", WhenNegative: "sales_product_charges"},
		{ID: 2, TransactionType: "ORDER", Description: "ANY", AmountField: "TOTAL",
			Template: "txn_ref+sku+date"},
		{ID: 3, TransactionType: "TRANSFER", Description: "TO_ACCOUNT_ENDING", AmountField: "TOTAL",
			Template: "TRANSFER+description+settlement_id+date"},
		{ID: 4, TransactionType: "SERVICE_FEE", Description: "SUBSCRIPTION", AmountField: "OTHER",
			Template: "SERVICE_FEE_SUBSCRIPTION+settlement_id+date",
			WhenPositive: "expenses_amazon_fees", WhenNegative: "expenses_amazon_fees"},
		// catch-all rules: empty transaction_type
		{ID: 5, TransactionType: "", Description: "ANY", AmountField: "TOTAL",
			Template: "txn_ref+settlement_id+date",
			WhenPositive: "expenses_amazon_fees", WhenNegative: "expenses_amazon_fees"},
		{ID: 6, TransactionType: "", Description: "ANY", AmountField: "OTHER",
			Template: "txn_ref+settlement_id+date"},
	})
}

func TestMatchExactBeatsWildcard(t *testing.T) {
	s := NewSet([]Rule{
		{ID: 1, TransactionType: "SERVICE_FEE", Description: "ANY", AmountField: "TOTAL", WhenPositive: "wildcard"},
		{ID: 2, TransactionType: "SERVICE_FEE", Description: "SUBSCRIPTION", AmountField: "TOTAL", WhenPositive: "exact"},
	})
	got := s.Match(Query{TransactionType: "SERVICE_FEE", Description: "SUBSCRIPTION", AmountField: "TOTAL"})
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("expected only the exact rule, got %+v", got)
	}
}

// "TO_ACCOUNT_ENDING" must match "TO_ACCOUNT_ENDING_WITH:_334".
func TestMatchPrefix(t *testing.T) {
	s := paymentRules()
	got := s.Match(Query{TransactionType: "TRANSFER", Description: "TO_ACCOUNT_ENDING_WITH:_334", AmountField: "TOTAL"})
	if len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("expected the prefix rule, got %+v", got)
	}
}

func TestMatchLongestPrefixWins(t *testing.T) {
	s := NewSet([]Rule{
		{ID: 1, TransactionType: "ADJUSTMENT", Description: "FBA_INVENTORY_REIMBURSEMENT", AmountField: "OTHER", WhenPositive: "short"},
		{ID: 2, TransactionType: "ADJUSTMENT", Description: "FBA_INVENTORY_REIMBURSEMENT_-_LOST", AmountField: "OTHER", WhenPositive: "long"},
	})
	got := s.Match(Query{TransactionType: "ADJUSTMENT", Description: "FBA_INVENTORY_REIMBURSEMENT_-_LOST:WAREHOUSE", AmountField: "OTHER"})
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("expected the longer prefix to win, got %+v", got)
	}
}

// The regression that produced 12,034 phantom unreconciled record_refs: a known
// transaction type with no rule for one amount column must NOT fall through to
// the catch-all rules, because those carry a different record_ref template.
func TestCatchAllOnlyForUnknownTransactionType(t *testing.T) {
	s := paymentRules()

	// ORDER is known but has no rule for the OTHER column -> no match at all.
	if got := s.Match(Query{TransactionType: "ORDER", Description: "ANY", AmountField: "OTHER"}); len(got) != 0 {
		t.Errorf("ORDER/OTHER should not reach the catch-all, got %+v", got)
	}

	// A type the config has never seen DOES reach the catch-all.
	got := s.Match(Query{TransactionType: "SOME_NEW_FEE", Description: "WHATEVER", AmountField: "TOTAL"})
	if len(got) != 1 || got[0].ID != 5 {
		t.Fatalf("unknown type should hit the catch-all, got %+v", got)
	}
}

// More than one match means the config double-routes an amount. The engine must
// surface all of them rather than silently choosing one.
func TestMatchReturnsEveryDuplicateRoute(t *testing.T) {
	s := NewSet([]Rule{
		{ID: 1, TransactionType: "ORDER", Description: "ANY", AmountField: "SALES_TAX_COLLECTED",
			WhenPositive: "sales_product_charges", WhenNegative: "sales_product_charges"},
		{ID: 2, TransactionType: "ORDER", Description: "ANY", AmountField: "SALES_TAX_COLLECTED",
			WhenPositive: "sales_shipping", WhenNegative: "sales_shipping"},
	})
	got := s.Match(Query{TransactionType: "ORDER", Description: "ANY", AmountField: "SALES_TAX_COLLECTED"})
	if len(got) != 2 {
		t.Fatalf("expected both duplicate rules, got %d", len(got))
	}
}

func TestAmountTypeMustMatch(t *testing.T) {
	s := NewSet([]Rule{
		{ID: 1, TransactionType: "ORDER", AmountType: "ITEMPRICE", Description: "PRINCIPAL", WhenPositive: "sales_product_charges"},
		{ID: 2, TransactionType: "ORDER", AmountType: "ITEMFEES", Description: "COMMISSION", WhenPositive: "expenses_amazon_fees"},
	})
	got := s.Match(Query{TransactionType: "ORDER", AmountType: "ITEMFEES", Description: "COMMISSION"})
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("amount_type must discriminate, got %+v", got)
	}
	if got := s.Match(Query{TransactionType: "ORDER", AmountType: "PROMOTION", Description: "PRINCIPAL"}); len(got) != 0 {
		t.Errorf("no rule for PROMOTION/PRINCIPAL, got %+v", got)
	}
}

// An `any` amount_description rule, e.g. COUPONREDEMPTIONFEE / any.
func TestAmountTypeWildcard(t *testing.T) {
	s := NewSet([]Rule{
		{ID: 1, TransactionType: "OTHER-TRANSACTION", AmountType: "INBOUND_DEFECT_FEE", Description: "ANY",
			WhenPositive: "expenses_fba_fees"},
	})
	got := s.Match(Query{TransactionType: "OTHER-TRANSACTION", AmountType: "INBOUND_DEFECT_FEE", Description: "ANYTHING_AT_ALL"})
	if len(got) != 1 {
		t.Fatalf("wildcard description should match, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// sign-based routing
// ---------------------------------------------------------------------------

func TestSummaryFieldBySign(t *testing.T) {
	r := Rule{WhenPositive: "sales_inventory_reimbursements", WhenNegative: "expenses_reversed_reimbursements"}
	if got := r.SummaryField(d("10.68")); got != "sales_inventory_reimbursements" {
		t.Errorf("positive -> %q", got)
	}
	if got := r.SummaryField(d("-10.68")); got != "expenses_reversed_reimbursements" {
		t.Errorf("negative -> %q", got)
	}
	// zero is routed as positive: it contributes nothing either way, but the
	// bucket choice must be deterministic
	if got := r.SummaryField(decimal.Zero); got != "sales_inventory_reimbursements" {
		t.Errorf("zero -> %q", got)
	}
}

func TestEmptyTargetMeansNotSummarised(t *testing.T) {
	r := Rule{WhenPositive: "", WhenNegative: ""}
	if r.SummaryField(d("-133756.51")) != "" {
		t.Error("an empty target must mean the amount does not contribute")
	}
}

// ---------------------------------------------------------------------------
// config linter
// ---------------------------------------------------------------------------

func TestLintFindsDuplicateRoutes(t *testing.T) {
	diags := Lint("PAYMENT", []Rule{
		{ID: 1, SourceLine: 71, TransactionType: "ORDER", Description: "ANY", AmountField: "SALES_TAX_COLLECTED",
			Template: "txn_ref+sku+date", WhenPositive: "sales_product_charges", WhenNegative: "sales_product_charges"},
		{ID: 2, SourceLine: 72, TransactionType: "ORDER", Description: "ANY", AmountField: "SALES_TAX_COLLECTED",
			Template: "txn_ref+sku+date", WhenPositive: "sales_shipping", WhenNegative: "sales_shipping"},
	})
	if len(diags) != 1 || diags[0].Kind != "DUPLICATE_ROUTE" {
		t.Fatalf("expected one DUPLICATE_ROUTE, got %+v", diags)
	}
	if len(diags[0].Lines) != 2 || diags[0].Lines[0] != 71 || diags[0].Lines[1] != 72 {
		t.Errorf("diagnostic should name both config lines, got %v", diags[0].Lines)
	}
}

func TestLintFindsTemplateConflicts(t *testing.T) {
	diags := Lint("SETTLEMENT", []Rule{
		{ID: 1, SourceLine: 10, TransactionType: "ORDER", AmountType: "ITEMPRICE", Description: "TAX",
			Template: "txn_ref+sku+date", WhenPositive: "sales_tax", WhenNegative: "sales_tax"},
		{ID: 2, SourceLine: 11, TransactionType: "ORDER", AmountType: "ITEMPRICE", Description: "TAX",
			Template: "txn_ref+settlement_id+date", WhenPositive: "sales_tax", WhenNegative: "sales_tax"},
	})
	if len(diags) != 1 || diags[0].Kind != "TEMPLATE_CONFLICT" {
		t.Fatalf("expected one TEMPLATE_CONFLICT, got %+v", diags)
	}
}

// Identical duplicate rows are harmless: same bucket, same template.
func TestLintIgnoresHarmlessDuplicates(t *testing.T) {
	diags := Lint("PAYMENT", []Rule{
		{ID: 1, SourceLine: 1, TransactionType: "ORDER", Description: "ANY", AmountField: "TOTAL", Template: "txn_ref+sku+date"},
		{ID: 2, SourceLine: 2, TransactionType: "ORDER", Description: "ANY", AmountField: "TOTAL", Template: "txn_ref+sku+date"},
	})
	if len(diags) != 0 {
		t.Errorf("identical rules should not be flagged, got %+v", diags)
	}
}
