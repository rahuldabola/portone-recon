// Package store owns everything that touches PostgreSQL: schema, bulk load of
// the ledger, the config tables, and the reconciliation queries.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rahuldabola/portone-recon/internal/mapping"
	"github.com/rahuldabola/portone-recon/internal/model"
	"github.com/shopspring/decimal"
)

type Store struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// ApplySchema runs the DDL file. It drops and recreates the tables, which is
// what makes a re-run idempotent.
func (s *Store) ApplySchema(ctx context.Context, path string) error {
	ddl, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, string(ddl))
	return err
}

// ApplyFile runs an arbitrary SQL file (used for MAPPING_FIXES.sql).
func (s *Store) ApplyFile(ctx context.Context, path string) error {
	sqlText, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, string(sqlText))
	return err
}

// ---------------------------------------------------------------------------
// config tables
// ---------------------------------------------------------------------------

func (s *Store) LoadPaymentConfig(ctx context.Context, rules []mapping.Rule) error {
	if _, err := s.pool.Exec(ctx, `TRUNCATE payment_config`); err != nil {
		return err
	}
	rows := make([][]any, 0, len(rules))
	for _, r := range rules {
		rows = append(rows, []any{r.ID, r.TransactionType, r.Description, r.AmountField,
			r.Template, r.WhenPositive, r.WhenNegative, r.SourceLine})
	}
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"payment_config"},
		[]string{"id", "transaction_type", "description", "amount_field",
			"record_ref_template", "summary_field_positive", "summary_field_negative", "source_line"},
		pgx.CopyFromRows(rows))
	return err
}

func (s *Store) LoadSettlementConfig(ctx context.Context, rules []mapping.Rule) error {
	if _, err := s.pool.Exec(ctx, `TRUNCATE settlement_config`); err != nil {
		return err
	}
	rows := make([][]any, 0, len(rules))
	for _, r := range rules {
		rows = append(rows, []any{r.ID, r.TransactionType, r.AmountType, r.Description,
			r.Template, r.WhenPositive, r.WhenNegative, r.SourceLine})
	}
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"settlement_config"},
		[]string{"id", "transaction_type", "amount_type", "amount_description",
			"record_ref_template", "summary_field_positive", "summary_field_negative", "source_line"},
		pgx.CopyFromRows(rows))
	return err
}

// ReadPaymentConfig reads the rules back out of the database, so that the
// pipeline runs off the *stored* config (and therefore off any fixes applied
// by MAPPING_FIXES.sql) rather than off the CSV.
func (s *Store) ReadPaymentConfig(ctx context.Context) ([]mapping.Rule, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, transaction_type, description, amount_field,
		record_ref_template, summary_field_positive, summary_field_negative, source_line
		FROM payment_config ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mapping.Rule
	for rows.Next() {
		var r mapping.Rule
		if err := rows.Scan(&r.ID, &r.TransactionType, &r.Description, &r.AmountField,
			&r.Template, &r.WhenPositive, &r.WhenNegative, &r.SourceLine); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ReadSettlementConfig(ctx context.Context) ([]mapping.Rule, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, transaction_type, amount_type, amount_description,
		record_ref_template, summary_field_positive, summary_field_negative, source_line
		FROM settlement_config ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mapping.Rule
	for rows.Next() {
		var r mapping.Rule
		if err := rows.Scan(&r.ID, &r.TransactionType, &r.AmountType, &r.Description,
			&r.Template, &r.WhenPositive, &r.WhenNegative, &r.SourceLine); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// ingest run + ledger
// ---------------------------------------------------------------------------

func (s *Store) StartRun(ctx context.Context, paymentsFile, paymentsSHA, settlementsFile, settlementsSHA, revision string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO ingest_run
		(payments_file, payments_sha256, settlements_file, settlements_sha256, config_revision)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		paymentsFile, paymentsSHA, settlementsFile, settlementsSHA, revision).Scan(&id)
	return id, err
}

func (s *Store) FinishRun(ctx context.Context, runID int64, payRows, setRows int, notes string) error {
	_, err := s.pool.Exec(ctx, `UPDATE ingest_run
		SET finished_at = now(), rows_payment = $2, rows_settlement = $3, notes = $4 WHERE id = $1`,
		runID, payRows, setRows, notes)
	return err
}

var ledgerColumns = []string{
	"run_id", "source", "source_file", "source_line", "raw_payload",
	"settlement_id", "txn_ref", "merchant_order_id", "adjustment_id", "shipment_id", "sku", "quantity",
	"transaction_type", "description", "amount_type", "amount_description", "amount_field",
	"transaction_type_raw", "description_raw", "marketplace", "fulfilment",
	"order_city", "order_state", "order_postal", "transaction_status",
	"amount", "posted_at", "released_at", "event_date",
	"record_ref", "summary_field", "config_id", "match_kind", "route_seq", "in_scope",
}

// LedgerWriter batches ledger rows into COPY calls so memory stays flat while
// streaming ~300k rows.
type LedgerWriter struct {
	s     *Store
	ctx   context.Context
	runID int64
	buf   [][]any
	size  int
	Total int64
}

func (s *Store) NewLedgerWriter(ctx context.Context, runID int64, batch int) *LedgerWriter {
	return &LedgerWriter{s: s, ctx: ctx, runID: runID, size: batch, buf: make([][]any, 0, batch)}
}

func (w *LedgerWriter) Add(e model.LedgerEntry) error {
	payload, err := json.Marshal(e.RawPayload)
	if err != nil {
		return err
	}
	var postedAt, releasedAt, eventDate any
	if !e.PostedAt.IsZero() {
		postedAt = e.PostedAt
	}
	if !e.ReleasedAt.IsZero() {
		releasedAt = e.ReleasedAt
	}
	if !e.EventDate.IsZero() {
		eventDate = e.EventDate
	}
	var qty any
	if e.Quantity != nil {
		qty = *e.Quantity
	}
	var cfg any
	if e.ConfigID != nil {
		cfg = *e.ConfigID
	}

	w.buf = append(w.buf, []any{
		w.runID, string(e.Source), e.SourceFile, e.SourceLine, payload,
		e.SettlementID, e.TxnRef, e.MerchantOrderID, e.AdjustmentID, e.ShipmentID, e.SKU, qty,
		e.TransactionType, e.Description, e.AmountType, e.AmountDescription, e.AmountField,
		e.TransactionTypeRaw, e.DescriptionRaw, e.Marketplace, e.Fulfilment,
		e.OrderCity, e.OrderState, e.OrderPostal, e.TransactionStatus,
		e.Amount, postedAt, releasedAt, eventDate,
		e.RecordRef, e.SummaryField, cfg, e.MatchKind, e.RouteSeq, e.InScope,
	})
	if len(w.buf) >= w.size {
		return w.Flush()
	}
	return nil
}

func (w *LedgerWriter) Flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	n, err := w.s.pool.CopyFrom(w.ctx, pgx.Identifier{"ledger_entry"}, ledgerColumns, pgx.CopyFromRows(w.buf))
	if err != nil {
		return err
	}
	w.Total += n
	w.buf = w.buf[:0]
	return nil
}

// WriteSummary persists the summary accumulated during ingestion.
func (s *Store) WriteSummary(ctx context.Context, runID int64, buckets map[model.Source]map[string]struct {
	Amount decimal.Decimal
	Count  int
}) error {
	rows := [][]any{}
	for src, m := range buckets {
		for field, b := range m {
			rows = append(rows, []any{runID, string(src), field, b.Amount, b.Count})
		}
	}
	if len(rows) == 0 {
		return nil
	}
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"summary_bucket"},
		[]string{"run_id", "source", "summary_field", "amount", "entry_count"}, pgx.CopyFromRows(rows))
	return err
}

func (s *Store) Pool() *pgxpool.Pool { return s.pool }
