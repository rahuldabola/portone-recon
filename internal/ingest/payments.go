package ingest

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/rahuldabola/portone-recon/internal/mapping"
	"github.com/rahuldabola/portone-recon/internal/model"
	"github.com/rahuldabola/portone-recon/internal/normalize"
	"github.com/shopspring/decimal"
)

// paymentAmountColumns maps each money column of the payments CSV to the
// `amount_field` name the payment config uses for it.
//
// Note "fulfilment by amazon fees" -> fba_fees and the two spellings Amazon
// uses across locales for the same column. The config also knows about
// `marketplace_withheld_tax`, which the AU export does not emit; the rules for
// it stay inert rather than erroring.
var paymentAmountColumns = []struct{ header, field string }{
	{"product sales", "PRODUCT_SALES"},
	{"product sales tax", "PRODUCT_SALES_TAX"},
	{"shipping credits", "SHIPPING_CREDITS"},
	{"shipping credits tax", "SHIPPING_CREDITS_TAX"},
	{"gift wrap credits", "GIFT_WRAP_CREDITS"},
	{"giftwrap credits tax", "GIFT_WRAP_CREDITS_TAX"},
	{"regulatory fee", "REGULATORY_FEE"},
	{"tax on regulatory fee", "TAX_ON_REGULATORY_FEE"},
	{"promotional rebates", "PROMOTIONAL_REBATES"},
	{"promotional rebates tax", "PROMOTIONAL_REBATES_TAX"},
	{"marketplace withheld tax", "MARKETPLACE_WITHHELD_TAX"},
	{"sales tax collected", "SALES_TAX_COLLECTED"},
	{"low value goods", "LOW_VALUE_GOODS"},
	{"selling fees", "SELLING_FEES"},
	{"fba fees", "FBA_FEES"},
	{"fulfilment by amazon fees", "FBA_FEES"},
	{"fulfillment by amazon fees", "FBA_FEES"},
	{"other transaction fees", "OTHER_TRANSACTION_FEES"},
	{"other", "OTHER"},
	{"total", "TOTAL"},
}

// ReadPayments streams the Amazon payments CSV, fanning each row out into one
// LedgerEntry per populated money column, resolving the mapping config as it
// goes. Nothing is written to disk or hand-cleaned first: the preamble skip,
// the thousands separators and the timestamp parsing all happen here.
func (ing *Ingester) ReadPayments(path string, emit func(model.LedgerEntry) error) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	br := bufio.NewReaderSize(stripBOM(f), 1<<20)
	r := csv.NewReader(br)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	var (
		header    []string
		headerIdx map[string]int
		line      int
		rows      int
	)
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return rows, fmt.Errorf("%s line %d: %w", path, line+1, err)
		}
		line++

		// The export opens with a free-text preamble ("Includes Amazon
		// Marketplace...", "Definitions:", ...). The real header is the first
		// row whose first cell is exactly "date/time".
		if header == nil {
			if len(rec) > 0 && strings.EqualFold(strings.TrimSpace(rec[0]), "date/time") {
				header = rec
				headerIdx = indexHeader(rec)
			}
			continue
		}
		if isBlank(rec) {
			continue
		}
		rows++
		if err := ing.emitPaymentRow(path, line, header, headerIdx, rec, emit); err != nil {
			return rows, err
		}
	}
	if header == nil {
		return 0, fmt.Errorf("%s: could not find the 'date/time' header row", path)
	}
	return rows, nil
}

func (ing *Ingester) emitPaymentRow(path string, line int, header []string, idx map[string]int, rec []string, emit func(model.LedgerEntry) error) error {
	get := func(name string) string {
		if i, ok := idx[name]; ok && i < len(rec) {
			return normalize.Text(rec[i])
		}
		return ""
	}

	raw := map[string]string{}
	for i, h := range header {
		if i < len(rec) {
			raw[h] = rec[i]
		}
	}

	postedAt, err := normalize.PaymentsTime(get("date/time"))
	if err != nil {
		return fmt.Errorf("%s line %d: %w", path, line, err)
	}
	releasedAt, err := normalize.PaymentsTime(get("transaction release date"))
	if err != nil {
		return fmt.Errorf("%s line %d: %w", path, line, err)
	}

	// The `date` token of a record_ref is the date the money actually entered a
	// settlement. On the payments side that is the Transaction Release Date
	// (converted to UTC), NOT the posted date/time: the settlement report's
	// posted-date is the release date. Deferred rows have no release date, so
	// they carry no event date and cannot join this settlement.
	eventDate := releasedAt

	base := model.LedgerEntry{
		Source:             model.SourcePayment,
		SourceFile:         baseName(path),
		SourceLine:         line,
		RawPayload:         raw,
		SettlementID:       get("settlement id"),
		TxnRef:             get("order id"),
		SKU:                get("sku"),
		TransactionTypeRaw: get("type"),
		DescriptionRaw:     get("description"),
		TransactionType:    normalize.Key(get("type")),
		Description:        normalize.Key(get("description")),
		Marketplace:        get("marketplace"),
		Fulfilment:         firstNonEmpty(get("fulfilment"), get("fulfillment")),
		OrderCity:          get("order city"),
		OrderState:         get("order state"),
		OrderPostal:        get("order postal"),
		TransactionStatus:  firstNonEmpty(get("transaction status"), get("transaction status ")),
		PostedAt:           postedAt,
		ReleasedAt:         releasedAt,
		EventDate:          eventDate,
	}
	if q := get("quantity"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			base.Quantity = &n
		}
	}

	seen := map[string]bool{}
	for _, col := range paymentAmountColumns {
		i, ok := idx[col.header]
		if !ok || seen[col.field] {
			continue
		}
		seen[col.field] = true
		amt, err := parseAmount(rec, i)
		if err != nil {
			return fmt.Errorf("%s line %d column %q: %w", path, line, col.header, err)
		}

		e := base
		e.AmountField = col.field
		e.Amount = amt

		q := mapping.Query{
			TransactionType: e.TransactionType,
			Description:     e.Description,
			AmountField:     e.AmountField,
		}
		if err := ing.resolve(&e, ing.PaymentRules, q, emit); err != nil {
			return err
		}
	}
	return nil
}

// parseAmount reads a money cell. Amazon writes "-1,234.56" with thousands
// separators and sometimes an empty cell for zero.
func parseAmount(rec []string, i int) (decimal.Decimal, error) {
	if i >= len(rec) {
		return decimal.Zero, nil
	}
	s := strings.TrimSpace(rec[i])
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, " ", "")
	if s == "" || s == "-" {
		return decimal.Zero, nil
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, fmt.Errorf("unparseable amount %q", rec[i])
	}
	return d, nil
}

func indexHeader(rec []string) map[string]int {
	m := map[string]int{}
	for i, h := range rec {
		k := strings.ToLower(strings.TrimSpace(h))
		if _, exists := m[k]; !exists {
			m[k] = i
		}
	}
	return m
}

func isBlank(rec []string) bool {
	for _, v := range rec {
		if strings.TrimSpace(v) != "" {
			return false
		}
	}
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func baseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
