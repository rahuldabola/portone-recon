// Package summary defines the accounting Summary sheet: which line items exist,
// how they are grouped and subtotalled, and which mapping-config summary field
// feeds each one.
//
// The line items and their order are taken verbatim from the Summary sheet of
// amazon_sample_output_report.xlsx (rows 4-32). The field -> line item mapping
// is derived from the two mapping configs: every `to_summary_field_when_*`
// value in either config must either name a line item below, or be listed in
// Unrouted (in which case the report shows it in an explicit "not on the
// Summary sheet" block rather than dropping it silently).
package summary

// Group is one of the three subtotalled sections plus the standalone
// "Paid To Amazon" line.
type Group string

const (
	GroupSales    Group = "Sales"
	GroupRefunds  Group = "Refunds"
	GroupExpenses Group = "Expenses"
	GroupPaid     Group = "Paid To Amazon"
)

// Line is one row of the Summary sheet.
type Line struct {
	Row     int    // 1-based row in the generated sheet, matching the sample
	Group   Group  // the subtotal this line rolls into
	Label   string // exactly as the sample spells it
	Field   string // mapping-config summary field feeding this line ("" for subtotal rows)
	IsTotal bool   // true for the group subtotal row itself
}

// Lines is the Summary sheet, in sheet order.
var Lines = []Line{
	{Row: 4, Group: GroupSales, Label: "Sales", IsTotal: true},
	{Row: 5, Group: GroupSales, Label: "Product Charges", Field: "sales_product_charges"},
	{Row: 6, Group: GroupSales, Label: "Tax", Field: "sales_tax"},
	{Row: 7, Group: GroupSales, Label: "Shipping", Field: "sales_shipping"},
	{Row: 8, Group: GroupSales, Label: "Amazon fees", Field: "sales_amazon_fees"},
	{Row: 9, Group: GroupSales, Label: "Inventory Reimbursements", Field: "sales_inventory_reimbursements"},
	{Row: 10, Group: GroupSales, Label: "Cross-account Debt Adjustment", Field: "sales_cross_account_debt_adjustment"},
	{Row: 11, Group: GroupSales, Label: "Other", Field: "sales_other"},
	{Row: 12, Group: GroupSales, Label: "FBA Fees", Field: "sales_fba_fees"},
	{Row: 13, Group: GroupSales, Label: "Micro Deposit (Failed)", Field: "sales_micro_deposit_failed"},

	{Row: 15, Group: GroupRefunds, Label: "Refunds", IsTotal: true},
	{Row: 16, Group: GroupRefunds, Label: "Refund expenses", Field: "refunded_expenses"},
	{Row: 17, Group: GroupRefunds, Label: "Refunded sales", Field: "refunded_sales"},

	{Row: 19, Group: GroupExpenses, Label: "Expenses", IsTotal: true},
	{Row: 20, Group: GroupExpenses, Label: "Promo rebates", Field: "expenses_promotional_rebates"},
	{Row: 21, Group: GroupExpenses, Label: "FBA fees", Field: "expenses_fba_fees"},
	{Row: 22, Group: GroupExpenses, Label: "Cost of Advertising", Field: "expenses_cost_of_advertising"},
	{Row: 23, Group: GroupExpenses, Label: "Shipping Charges", Field: "expenses_shipping_charges"},
	{Row: 24, Group: GroupExpenses, Label: "Amazon fees", Field: "expenses_amazon_fees"},
	{Row: 25, Group: GroupExpenses, Label: "Reversed Reimbursements", Field: "expenses_reversed_reimbursements"},
	{Row: 26, Group: GroupExpenses, Label: "Cross-account Debt Adjustment", Field: "expenses_cross_account_debt_adjustment"},
	{Row: 27, Group: GroupExpenses, Label: "Other", Field: "expenses_other"},
	{Row: 28, Group: GroupExpenses, Label: "Micro Deposit", Field: "bank_account_transfer_round_off"},

	{Row: 32, Group: GroupPaid, Label: "Paid To Amazon", Field: "paid_to_amazon", IsTotal: true},
}

// Unrouted lists summary fields that appear in the mapping configs but have no
// line on the Summary sheet. They are *balances*, not period movements:
// beginning balance, closing reserve and the amount carried forward to the next
// settlement describe the account's position, not money earned or spent inside
// this settlement, so including them in Sales/Refunds/Expenses would double
// count the period.
//
// Anything else that reaches the report without a line item is a config defect,
// and the report prints it under "Summary fields with no Summary line".
var Unrouted = map[string]string{
	"beginning_balance":      "opening reserve balance carried in from the previous settlement",
	"current_reserve_amount": "closing reserve balance held back by Amazon",
	"amazon_carried_forward": "net amount carried forward to the next settlement",
}

// FieldToLine indexes the line items by config field name.
var FieldToLine = func() map[string]Line {
	m := map[string]Line{}
	for _, l := range Lines {
		if l.Field != "" {
			m[l.Field] = l
		}
	}
	return m
}()

// GroupRows gives the member (non-subtotal) rows of each group, for building
// the subtotal formulas.
var GroupRows = func() map[Group][]int {
	m := map[Group][]int{}
	for _, l := range Lines {
		if l.IsTotal || l.Field == "" {
			continue
		}
		m[l.Group] = append(m[l.Group], l.Row)
	}
	return m
}()

// ConsolidatedColumns are the per-summary-field money columns repeated on the
// Consolidated sheet for each source, in sheet order. "Total" is the row total
// and is not a summary field, hence the empty Field.
type ConsolidatedColumn struct {
	Header string
	Group  string
	Field  string
}

var ConsolidatedColumns = []ConsolidatedColumn{
	{"Total", "", ""},
	{"Product Charges", "Sales", "sales_product_charges"},
	{"Tax", "Sales", "sales_tax"},
	{"Shipping", "Sales", "sales_shipping"},
	{"Amazon Fees", "Sales", "sales_amazon_fees"},
	{"Inventory Reimbursements", "Sales", "sales_inventory_reimbursements"},
	{"Cross-Account Debt Adjustment", "Sales", "sales_cross_account_debt_adjustment"},
	{"Other", "Sales", "sales_other"},
	{"FBA Fees", "Sales", "sales_fba_fees"},
	{"Micro Deposit (Failed)", "Sales", "sales_micro_deposit_failed"},
	{"Refund Expenses", "Refunds", "refunded_expenses"},
	{"Refunded Sales", "Refunds", "refunded_sales"},
	{"Promo Rebates", "Expenses", "expenses_promotional_rebates"},
	{"FBA Fees", "Expenses", "expenses_fba_fees"},
	{"Cost of Advertising", "Expenses", "expenses_cost_of_advertising"},
	{"Shipping Charges", "Expenses", "expenses_shipping_charges"},
	{"Amazon Fees", "Expenses", "expenses_amazon_fees"},
	{"Reversed Reimbursements", "Expenses", "expenses_reversed_reimbursements"},
	{"Cross-Account Debt Adjustment", "Expenses", "expenses_cross_account_debt_adjustment"},
	{"Other", "Expenses", "expenses_other"},
	{"Micro Deposit", "Expenses", "bank_account_transfer_round_off"},
	{"Paid To Amazon", "Paid To Amazon", "paid_to_amazon"},
}
