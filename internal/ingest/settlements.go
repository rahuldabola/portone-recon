package ingest

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rahuldabola/portone-recon/internal/mapping"
	"github.com/rahuldabola/portone-recon/internal/model"
	"github.com/rahuldabola/portone-recon/internal/normalize"
)

// ReadSettlements streams the tab-separated settlement flat file.
//
// The file's first data row is a settlement *header*: settlement id, period,
// deposit date and the settlement total, with every transaction column blank.
// It is captured as the settlement of record (and is what the report ties back
// to) but produces no ledger amount of its own.
func (ing *Ingester) ReadSettlements(path string, emit func(model.LedgerEntry) error) (int, []model.Settlement, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(stripBOM(f))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<22)

	var (
		header   []string
		idx      map[string]int
		line     int
		rows     int
		settles  []model.Settlement
	)
	for sc.Scan() {
		line++
		fields := strings.Split(strings.TrimRight(sc.Text(), "\r"), "\t")
		if line == 1 {
			header = fields
			idx = indexHeader(fields)
			continue
		}
		if isBlank(fields) {
			continue
		}
		rows++

		get := func(name string) string {
			if i, ok := idx[name]; ok && i < len(fields) {
				return normalize.Text(fields[i])
			}
			return ""
		}
		raw := map[string]string{}
		for i, h := range header {
			if i < len(fields) {
				raw[h] = fields[i]
			}
		}

		// Settlement header row: no transaction-type, but a total-amount.
		if get("transaction-type") == "" && get("amount-type") == "" && get("total-amount") != "" {
			start, _ := normalize.SettlementTime(get("settlement-start-date"))
			end, _ := normalize.SettlementTime(get("settlement-end-date"))
			dep, _ := normalize.SettlementTime(get("deposit-date"))
			total, err := parseAmount(fields, idx["total-amount"])
			if err != nil {
				return rows, settles, fmt.Errorf("%s line %d: %w", path, line, err)
			}
			settles = append(settles, model.Settlement{
				ID: get("settlement-id"), StartDate: start, EndDate: end,
				DepositDate: dep, TotalAmount: total, Currency: get("currency"),
			})
			continue
		}

		postedAt, err := normalize.SettlementTime(firstNonEmpty(get("posted-date-time"), get("posted-date")))
		if err != nil {
			return rows, settles, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		eventDate, err := normalize.SettlementTime(get("posted-date"))
		if err != nil {
			return rows, settles, fmt.Errorf("%s line %d: %w", path, line, err)
		}

		amt, err := parseAmount(fields, idx["amount"])
		if err != nil {
			return rows, settles, fmt.Errorf("%s line %d: %w", path, line, err)
		}

		e := model.LedgerEntry{
			Source:             model.SourceSettlement,
			SourceFile:         baseName(path),
			SourceLine:         line,
			RawPayload:         raw,
			SettlementID:       get("settlement-id"),
			TxnRef:             get("order-id"),
			MerchantOrderID:    get("merchant-order-id"),
			AdjustmentID:       get("adjustment-id"),
			ShipmentID:         get("shipment-id"),
			SKU:                get("sku"),
			TransactionTypeRaw: get("transaction-type"),
			DescriptionRaw:     get("amount-description"),
			TransactionType:    normalize.Key(get("transaction-type")),
			AmountType:         normalize.Key(get("amount-type")),
			AmountDescription:  normalize.Key(get("amount-description")),
			Description:        normalize.Key(get("amount-description")),
			Marketplace:        get("marketplace-name"),
			Fulfilment:         get("fulfillment-id"),
			Amount:             amt,
			PostedAt:           postedAt,
			EventDate:          eventDate,
		}
		if q := get("quantity-purchased"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				e.Quantity = &n
			}
		}

		q := mapping.Query{
			TransactionType: e.TransactionType,
			Description:     e.AmountDescription,
			AmountType:      e.AmountType,
		}
		if err := ing.resolve(&e, ing.SettlementRules, q, emit); err != nil {
			return rows, settles, err
		}
	}
	if err := sc.Err(); err != nil {
		return rows, settles, fmt.Errorf("%s: %w", path, err)
	}
	return rows, settles, nil
}
