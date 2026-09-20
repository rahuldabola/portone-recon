package ingest

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rahuldabola/portone-recon/internal/mapping"
	"github.com/rahuldabola/portone-recon/internal/normalize"
)

// LoadPaymentConfig reads amazon_payment_configs_*.csv into rules.
//
// Columns: transaction_type, description, amount_field, record_ref,
//          to_summary_field_when_positive_amount, to_summary_field_when_negative_amount
func LoadPaymentConfig(path string) ([]mapping.Rule, error) {
	recs, err := readCSV(path, 6)
	if err != nil {
		return nil, err
	}
	var rules []mapping.Rule
	for i, rec := range recs {
		rules = append(rules, mapping.Rule{
			ID:              i + 1,
			SourceLine:      i + 2, // +1 for header, +1 for 1-based
			TransactionType: normalize.Key(rec.fields[0]),
			Description:     normalize.Key(rec.fields[1]),
			AmountField:     normalize.Key(rec.fields[2]),
			Template:        strings.TrimSpace(rec.fields[3]),
			WhenPositive:    strings.TrimSpace(rec.fields[4]),
			WhenNegative:    strings.TrimSpace(rec.fields[5]),
		})
	}
	return rules, nil
}

// LoadSettlementConfig reads amazon_settlement_configs_*.csv into rules.
//
// Columns: transaction_type, amount_type, amount_description, record_ref,
//          to_summary_field_when_positive_amount, to_summary_field_when_negative_amount
func LoadSettlementConfig(path string) ([]mapping.Rule, error) {
	recs, err := readCSV(path, 6)
	if err != nil {
		return nil, err
	}
	var rules []mapping.Rule
	for i, rec := range recs {
		rules = append(rules, mapping.Rule{
			ID:              i + 1,
			SourceLine:      i + 2,
			TransactionType: normalize.Key(rec.fields[0]),
			AmountType:      normalize.Key(rec.fields[1]),
			Description:     normalize.Key(rec.fields[2]),
			Template:        strings.TrimSpace(rec.fields[3]),
			WhenPositive:    strings.TrimSpace(rec.fields[4]),
			WhenNegative:    strings.TrimSpace(rec.fields[5]),
		})
	}
	return rules, nil
}

type csvRecord struct {
	line   int
	fields []string
}

func readCSV(path string, want int) ([]csvRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(stripBOM(f))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	var out []csvRecord
	line := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		line++
		if line == 1 {
			continue // header
		}
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}
		for len(rec) < want {
			rec = append(rec, "")
		}
		out = append(out, csvRecord{line: line, fields: rec})
	}
	return out, nil
}
