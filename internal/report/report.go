// Package report writes the accounting workbook: a Summary sheet that follows
// the structure of amazon_sample_output_report.xlsx, and a Consolidated Data
// sheet that shows every record on both sides with the difference.
package report

import (
	"fmt"
	"sort"

	"github.com/rahuldabola/portone-recon/internal/recon"
	"github.com/rahuldabola/portone-recon/internal/summary"
	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"
)

// Input is everything the workbook needs.
type Input struct {
	Title            string
	SettlementID     string
	SettlementPeriod string
	DepositDate      string
	Currency         string
	AmazonTotal      decimal.Decimal // the settlement report's own total-amount

	// Summary buckets accumulated during ingest, by source then field.
	PaymentSummary    map[string]decimal.Decimal
	SettlementSummary map[string]decimal.Decimal

	Recon *recon.Result

	// Diagnostics
	ConfigDiagnostics []string
	UnmappedRows      []string
	ScopeNotes        []string
}

// Write renders the workbook to path.
func Write(path string, in Input) error {
	f := excelize.NewFile()
	defer f.Close()

	if err := writeSummary(f, in); err != nil {
		return err
	}
	if err := writeConsolidated(f, in); err != nil {
		return err
	}
	f.SetActiveSheet(0)
	return f.SaveAs(path)
}

// ---------------------------------------------------------------------------
// Summary sheet
// ---------------------------------------------------------------------------

func writeSummary(f *excelize.File, in Input) error {
	const sheet = "Summary"
	if err := f.SetSheetName("Sheet1", sheet); err != nil {
		return err
	}

	money, _ := f.NewStyle(&excelize.Style{NumFmt: 4}) // #,##0.00
	bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	boldMoney, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true}, NumFmt: 4,
		Border: []excelize.Border{{Type: "top", Color: "000000", Style: 1}},
	})
	head, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true},
		Alignment: &excelize.Alignment{Horizontal: "center"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"EEEEEE"}},
	})

	_ = f.SetColWidth(sheet, "A", "A", 3)
	_ = f.SetColWidth(sheet, "B", "B", 34)
	_ = f.SetColWidth(sheet, "C", "E", 22)

	_ = f.SetCellValue(sheet, "C1", "Payments")
	_ = f.SetCellValue(sheet, "D1", "Settlements")
	_ = f.SetCellValue(sheet, "E1", "Payments - Settlements")
	_ = f.SetCellStyle(sheet, "C1", "E1", head)

	groupTotals := map[summary.Group][2]decimal.Decimal{}

	for _, l := range summary.Lines {
		cellB := fmt.Sprintf("B%d", l.Row)
		_ = f.SetCellValue(sheet, cellB, l.Label)

		if l.IsTotal && l.Field == "" {
			// Subtotal row: sum of the member rows, as a live formula so a
			// finance user can see the arithmetic.
			rows := summary.GroupRows[l.Group]
			if len(rows) > 0 {
				first, last := rows[0], rows[len(rows)-1]
				for _, col := range []string{"C", "D", "E"} {
					_ = f.SetCellFormula(sheet, fmt.Sprintf("%s%d", col, l.Row),
						fmt.Sprintf("SUM(%s%d:%s%d)", col, first, col, last))
				}
			}
			_ = f.SetCellStyle(sheet, cellB, cellB, bold)
			_ = f.SetCellStyle(sheet, fmt.Sprintf("C%d", l.Row), fmt.Sprintf("E%d", l.Row), boldMoney)
			continue
		}

		p := in.PaymentSummary[l.Field]
		s := in.SettlementSummary[l.Field]
		_, _ = p, s
		_ = f.SetCellValue(sheet, fmt.Sprintf("C%d", l.Row), toFloat(p))
		_ = f.SetCellValue(sheet, fmt.Sprintf("D%d", l.Row), toFloat(s))
		_ = f.SetCellFormula(sheet, fmt.Sprintf("E%d", l.Row), fmt.Sprintf("C%d-D%d", l.Row, l.Row))
		style := money
		if l.IsTotal {
			style = boldMoney
			_ = f.SetCellStyle(sheet, cellB, cellB, bold)
		}
		_ = f.SetCellStyle(sheet, fmt.Sprintf("C%d", l.Row), fmt.Sprintf("E%d", l.Row), style)

		g := groupTotals[l.Group]
		groupTotals[l.Group] = [2]decimal.Decimal{g[0].Add(p), g[1].Add(s)}
	}

	// ---- tie-out block -----------------------------------------------------
	row := 34
	put := func(label, value string) {
		_ = f.SetCellValue(sheet, fmt.Sprintf("B%d", row), label)
		_ = f.SetCellValue(sheet, fmt.Sprintf("C%d", row), value)
		row++
	}
	putMoney := func(label string, v decimal.Decimal) {
		_ = f.SetCellValue(sheet, fmt.Sprintf("B%d", row), label)
		_ = f.SetCellValue(sheet, fmt.Sprintf("C%d", row), toFloat(v))
		_ = f.SetCellStyle(sheet, fmt.Sprintf("C%d", row), fmt.Sprintf("C%d", row), money)
		row++
	}

	_ = f.SetCellValue(sheet, fmt.Sprintf("B%d", row), "Tie-out to Amazon")
	_ = f.SetCellStyle(sheet, fmt.Sprintf("B%d", row), fmt.Sprintf("B%d", row), bold)
	row++
	put("Settlement ID", in.SettlementID)
	put("Settlement period", in.SettlementPeriod)
	put("Deposit date", in.DepositDate)
	put("Currency", in.Currency)
	putMoney("Amazon's reported settlement total", in.AmazonTotal)

	var payTotal, setTotal decimal.Decimal
	for _, l := range summary.Lines {
		if l.Field == "" {
			continue
		}
		payTotal = payTotal.Add(in.PaymentSummary[l.Field])
		setTotal = setTotal.Add(in.SettlementSummary[l.Field])
	}
	putMoney("Summary total — Payments column", payTotal)
	putMoney("Summary total — Settlements column", setTotal)
	putMoney("Variance vs Amazon (Payments)", payTotal.Sub(in.AmazonTotal))
	putMoney("Variance vs Amazon (Settlements)", setTotal.Sub(in.AmazonTotal))
	row++

	_ = f.SetCellValue(sheet, fmt.Sprintf("B%d", row), "Reconciliation counts")
	_ = f.SetCellStyle(sheet, fmt.Sprintf("B%d", row), fmt.Sprintf("B%d", row), bold)
	row++
	if in.Recon != nil {
		put("Reconciled record_refs", fmt.Sprint(in.Recon.CountReconciled))
		put("Unreconciled — payment only", fmt.Sprint(in.Recon.CountUnrecPayment))
		put("Unreconciled — settlement only", fmt.Sprint(in.Recon.CountUnrecSettlement))
		put("Payment ledger rows in scope", fmt.Sprint(in.Recon.PaymentRowsInScope))
		put("Settlement ledger rows in scope", fmt.Sprint(in.Recon.SettlementRowsInScope))
	}
	row++

	// ---- summary fields with no Summary line -------------------------------
	orphans := orphanFields(in)
	_ = f.SetCellValue(sheet, fmt.Sprintf("B%d", row), "Summary fields with no Summary line")
	_ = f.SetCellStyle(sheet, fmt.Sprintf("B%d", row), fmt.Sprintf("B%d", row), bold)
	row++
	if len(orphans) == 0 {
		put("(none)", "")
	} else {
		_ = f.SetCellValue(sheet, fmt.Sprintf("C%d", row), "Payments")
		_ = f.SetCellValue(sheet, fmt.Sprintf("D%d", row), "Settlements")
		_ = f.SetCellValue(sheet, fmt.Sprintf("E%d", row), "Note")
		_ = f.SetCellStyle(sheet, fmt.Sprintf("C%d", row), fmt.Sprintf("E%d", row), head)
		row++
		for _, o := range orphans {
			_ = f.SetCellValue(sheet, fmt.Sprintf("B%d", row), o.field)
			_ = f.SetCellValue(sheet, fmt.Sprintf("C%d", row), toFloat(o.pay))
			_ = f.SetCellValue(sheet, fmt.Sprintf("D%d", row), toFloat(o.set))
			_ = f.SetCellValue(sheet, fmt.Sprintf("E%d", row), o.note)
			_ = f.SetCellStyle(sheet, fmt.Sprintf("C%d", row), fmt.Sprintf("D%d", row), money)
			row++
		}
	}
	row++

	for _, block := range []struct {
		title string
		lines []string
	}{
		{"Scope", in.ScopeNotes},
		{"Config diagnostics", in.ConfigDiagnostics},
		{"Rows matching no config rule", in.UnmappedRows},
	} {
		_ = f.SetCellValue(sheet, fmt.Sprintf("B%d", row), block.title)
		_ = f.SetCellStyle(sheet, fmt.Sprintf("B%d", row), fmt.Sprintf("B%d", row), bold)
		row++
		if len(block.lines) == 0 {
			_ = f.SetCellValue(sheet, fmt.Sprintf("B%d", row), "(none)")
			row++
		}
		for _, l := range block.lines {
			_ = f.SetCellValue(sheet, fmt.Sprintf("B%d", row), l)
			row++
		}
		row++
	}
	return nil
}

type orphan struct {
	field    string
	pay, set decimal.Decimal
	note     string
}

// orphanFields lists summary fields that carry money but have no line on the
// Summary sheet. Anything here is either a known balance field or a defect.
func orphanFields(in Input) []orphan {
	seen := map[string]bool{}
	for f := range in.PaymentSummary {
		seen[f] = true
	}
	for f := range in.SettlementSummary {
		seen[f] = true
	}
	var out []orphan
	for field := range seen {
		if _, ok := summary.FieldToLine[field]; ok {
			continue
		}
		note, known := summary.Unrouted[field]
		if !known {
			note = "NOT a Summary line item — config routes money into a bucket the report cannot show"
		}
		p, s := in.PaymentSummary[field], in.SettlementSummary[field]
		if p.IsZero() && s.IsZero() {
			continue
		}
		out = append(out, orphan{field: field, pay: p, set: s, note: note})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].field < out[j].field })
	return out
}

// ---------------------------------------------------------------------------
// Consolidated Data sheet
// ---------------------------------------------------------------------------

var paymentIdentityCols = []string{
	"Date", "date/time", "Transaction Status", "Transaction Release Date", "settlement id",
	"type", "order id", "sku", "description", "Quantity", "marketplace", "account type",
	"fulfillment", "order city", "order state", "order postal", "tax collection model",
}

var settlementIdentityCols = []string{
	"settlement-id", "settlement-start-date", "settlement-end-date", "deposit-date",
	"total-amount", "currency", "transaction-type", "order-id", "merchant-order-id",
	"adjustment-id", "shipment-id", "marketplace-name", "fulfillment-id", "posted-date",
	"posted-date-time", "order-item-code", "merchant-order-item-id",
	"merchant-adjustment-item-id", "sku", "quantity", "promotion-id",
}

var auditCols = []string{
	"record_ref", "payment rows", "settlement rows",
	"payment source file", "payment source lines", "settlement source file", "settlement source lines",
	"payment summary field (sample)", "settlement summary field (sample)", "payment match kind", "settlement match kind",
}

func writeConsolidated(f *excelize.File, in Input) error {
	const sheet = "Consolidated Data"
	if _, err := f.NewSheet(sheet); err != nil {
		return err
	}
	sw, err := f.NewStreamWriter(sheet)
	if err != nil {
		return err
	}

	bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	band, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true},
		Alignment: &excelize.Alignment{Horizontal: "center"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"DDEBF7"}},
	})
	money, _ := f.NewStyle(&excelize.Style{NumFmt: 4})

	// ---- banner rows (mirroring the sample) --------------------------------
	_ = sw.SetRow("A1", []any{excelize.Cell{StyleID: bold, Value: "      Data From Amazon"}})
	_ = sw.SetRow("A3", []any{excelize.Cell{Value: "DS1 ="}, excelize.Cell{Value: "Payment Report"}})
	_ = sw.SetRow("A4", []any{excelize.Cell{Value: "DS2 ="}, excelize.Cell{Value: "Settlement Report"}})
	_ = sw.SetRow("A5", []any{excelize.Cell{Value: fmt.Sprintf("Settlement %s  |  %s  |  deposit %s  |  %s",
		in.SettlementID, in.SettlementPeriod, in.DepositDate, in.Currency)}})

	nPayID, nSetID := len(paymentIdentityCols), len(settlementIdentityCols)
	nCols := len(summary.ConsolidatedColumns)

	payStart := 1
	paySumStart := payStart + nPayID
	setStart := paySumStart + nCols
	setSumStart := setStart + nSetID
	statusCol := setSumStart + nCols
	diffStart := statusCol + 1
	auditStart := diffStart + nCols
	totalCols := auditStart + len(auditCols) - 1

	// row 7: source band
	row7 := make([]any, totalCols)
	for i := payStart; i < setStart; i++ {
		row7[i-1] = excelize.Cell{StyleID: band, Value: "Payment Report"}
	}
	for i := setStart; i < statusCol; i++ {
		row7[i-1] = excelize.Cell{StyleID: band, Value: "Settlement Report"}
	}
	for i := statusCol; i < auditStart; i++ {
		row7[i-1] = excelize.Cell{StyleID: band, Value: "Reconciliation"}
	}
	for i := auditStart; i <= totalCols; i++ {
		row7[i-1] = excelize.Cell{StyleID: band, Value: "Audit trail"}
	}
	_ = sw.SetRow("A7", row7)

	// row 8: "Summary" / "Summary Mismatch" band
	row8 := make([]any, totalCols)
	for i := paySumStart; i < paySumStart+nCols; i++ {
		row8[i-1] = excelize.Cell{StyleID: band, Value: "Summary"}
	}
	for i := setSumStart; i < setSumStart+nCols; i++ {
		row8[i-1] = excelize.Cell{StyleID: band, Value: "Summary"}
	}
	for i := diffStart; i < diffStart+nCols; i++ {
		row8[i-1] = excelize.Cell{StyleID: band, Value: "Summary Mismatch"}
	}
	_ = sw.SetRow("A8", row8)

	// row 9: group band over the summary columns
	row9 := make([]any, totalCols)
	for j, c := range summary.ConsolidatedColumns {
		if c.Group == "" {
			continue
		}
		row9[paySumStart+j-1] = excelize.Cell{StyleID: band, Value: c.Group}
		row9[setSumStart+j-1] = excelize.Cell{StyleID: band, Value: c.Group}
		row9[diffStart+j-1] = excelize.Cell{StyleID: band, Value: c.Group}
	}
	_ = sw.SetRow("A9", row9)

	// row 10: the real header
	hdr := make([]any, totalCols)
	for j, h := range paymentIdentityCols {
		hdr[payStart+j-1] = excelize.Cell{StyleID: bold, Value: h}
	}
	for j, c := range summary.ConsolidatedColumns {
		hdr[paySumStart+j-1] = excelize.Cell{StyleID: bold, Value: c.Header}
	}
	for j, h := range settlementIdentityCols {
		hdr[setStart+j-1] = excelize.Cell{StyleID: bold, Value: h}
	}
	for j, c := range summary.ConsolidatedColumns {
		hdr[setSumStart+j-1] = excelize.Cell{StyleID: bold, Value: c.Header}
	}
	hdr[statusCol-1] = excelize.Cell{StyleID: bold, Value: "Reconciliation Status"}
	for j, c := range summary.ConsolidatedColumns {
		label := c.Header
		if j == 0 {
			label = "total"
		}
		hdr[diffStart+j-1] = excelize.Cell{StyleID: bold, Value: label + " (DS1-DS2)"}
	}
	for j, h := range auditCols {
		hdr[auditStart+j-1] = excelize.Cell{StyleID: bold, Value: h}
	}
	_ = sw.SetRow("A10", hdr)

	// ---- data --------------------------------------------------------------
	rowNum := 11
	for _, p := range in.Recon.Pairs {
		cells := make([]any, totalCols)

		if s := p.Payment; s != nil {
			raw := s.Raw
			vals := []string{
				s.Attrs["event_date"], raw["date/time"], raw["Transaction status"],
				raw["Transaction Release Date"], s.Attrs["settlement_id"], s.Attrs["type"],
				s.Attrs["txn_ref"], s.Attrs["sku"], s.Attrs["description"], s.Attrs["quantity"],
				s.Attrs["marketplace"], raw["account type"], s.Attrs["fulfilment"],
				s.Attrs["order_city"], s.Attrs["order_state"], s.Attrs["order_postal"],
				raw["tax collection model"],
			}
			for j, v := range vals {
				cells[payStart+j-1] = v
			}
			for j, c := range summary.ConsolidatedColumns {
				v := s.Total
				if c.Field != "" {
					v = s.Field(c.Field)
				} else if j != 0 {
					continue
				}
				if v.IsZero() {
					continue
				}
				cells[paySumStart+j-1] = excelize.Cell{StyleID: money, Value: toFloat(v)}
			}
		}

		if s := p.Settlement; s != nil {
			raw := s.Raw
			vals := []string{
				s.Attrs["settlement_id"], raw["settlement-start-date"], raw["settlement-end-date"],
				raw["deposit-date"], raw["total-amount"], raw["currency"], s.Attrs["type"],
				s.Attrs["txn_ref"], raw["merchant-order-id"], raw["adjustment-id"], raw["shipment-id"],
				s.Attrs["marketplace"], raw["fulfillment-id"], raw["posted-date"], raw["posted-date-time"],
				raw["order-item-code"], raw["merchant-order-item-id"], raw["merchant-adjustment-item-id"],
				s.Attrs["sku"], s.Attrs["quantity"], raw["promotion-id"],
			}
			for j, v := range vals {
				cells[setStart+j-1] = v
			}
			for j, c := range summary.ConsolidatedColumns {
				v := s.Total
				if c.Field != "" {
					v = s.Field(c.Field)
				} else if j != 0 {
					continue
				}
				if v.IsZero() {
					continue
				}
				cells[setSumStart+j-1] = excelize.Cell{StyleID: money, Value: toFloat(v)}
			}
		}

		cells[statusCol-1] = string(p.Status)

		for j, c := range summary.ConsolidatedColumns {
			var d decimal.Decimal
			if c.Field == "" {
				if j != 0 {
					continue
				}
				d = p.TotalDiff()
			} else {
				d = p.Diff(c.Field)
			}
			if d.IsZero() {
				continue
			}
			cells[diffStart+j-1] = excelize.Cell{StyleID: money, Value: toFloat(d)}
		}

		audit := []string{
			p.RecordRef,
			countOf(p.Payment), countOf(p.Settlement),
			attr(p.Payment, "source_file"), lines(p.Payment),
			attr(p.Settlement, "source_file"), lines(p.Settlement),
			attr(p.Payment, "summary_field"), attr(p.Settlement, "summary_field"),
			attr(p.Payment, "match_kind"), attr(p.Settlement, "match_kind"),
		}
		for j, v := range audit {
			cells[auditStart+j-1] = v
		}

		if err := sw.SetRow(fmt.Sprintf("A%d", rowNum), cells); err != nil {
			return err
		}
		rowNum++
	}

	_ = sw.SetColWidth(payStart, payStart+nPayID-1, 18)
	_ = sw.SetColWidth(paySumStart, statusCol, 16)
	_ = sw.SetColWidth(auditStart, totalCols, 26)
	return sw.Flush()
}

func countOf(s *recon.Side) string {
	if s == nil {
		return ""
	}
	return fmt.Sprint(s.Rows)
}

func attr(s *recon.Side, k string) string {
	if s == nil {
		return ""
	}
	return s.Attrs[k]
}

func lines(s *recon.Side) string {
	if s == nil || len(s.SourceLines) == 0 {
		return ""
	}
	out := ""
	for i, l := range s.SourceLines {
		if i > 0 {
			out += ","
		}
		if i == 20 {
			out += "…"
			break
		}
		out += fmt.Sprint(l)
	}
	return out
}

func toFloat(d decimal.Decimal) float64 {
	v, _ := d.Round(2).Float64()
	return v
}
