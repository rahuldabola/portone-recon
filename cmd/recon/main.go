// Command recon runs the Amazon Payments vs Settlement reconciliation:
//
//	recon ingest  -- load configs + both reports into PostgreSQL
//	recon report  -- reconcile and write the .xlsx
//	recon all     -- both, which is the normal end-to-end run
//
// See README.md for the full flow.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rahuldabola/portone-recon/internal/ingest"
	"github.com/rahuldabola/portone-recon/internal/mapping"
	"github.com/rahuldabola/portone-recon/internal/model"
	"github.com/rahuldabola/portone-recon/internal/recon"
	"github.com/rahuldabola/portone-recon/internal/report"
	"github.com/rahuldabola/portone-recon/internal/store"
	"github.com/rahuldabola/portone-recon/internal/summary"
	"github.com/shopspring/decimal"
)

type options struct {
	dsn              string
	schema           string
	payments         string
	settlements      string
	paymentConfig    string
	settlementConfig string
	fixes            string
	out              string
	revision         string
	runID            int64
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: recon <ingest|report|all> [flags]")
	}
	cmd := os.Args[1]

	var o options
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.StringVar(&o.dsn, "dsn", envOr("RECON_DSN", "postgres://postgres:postgres@127.0.0.1:5432/portone_recon"), "PostgreSQL DSN")
	fs.StringVar(&o.schema, "schema", "sql/001_schema.sql", "path to schema DDL")
	fs.StringVar(&o.payments, "payments", "data/amazon_payments_data.csv", "Amazon payments report")
	fs.StringVar(&o.settlements, "settlements", "data/amazon_settlements_data.txt", "Amazon settlement report")
	fs.StringVar(&o.paymentConfig, "payment-config", "data/amazon_payment_configs_au_old.csv", "payment mapping config")
	fs.StringVar(&o.settlementConfig, "settlement-config", "data/amazon_settlement_configs_au.csv", "settlement mapping config")
	fs.StringVar(&o.fixes, "fixes", "", "optional MAPPING_FIXES.sql applied to the config tables before ingest")
	fs.StringVar(&o.out, "out", "out/report.xlsx", "output .xlsx path")
	fs.StringVar(&o.revision, "revision", "before-fix", "label recorded on the ingest run")
	fs.Int64Var(&o.runID, "run", 0, "existing run id to report on (report only)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}

	ctx := context.Background()
	st, err := store.Open(ctx, o.dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	switch cmd {
	case "ingest":
		_, err := doIngest(ctx, st, o)
		return err
	case "report":
		if o.runID == 0 {
			return fmt.Errorf("report needs -run <id> (or use `all`)")
		}
		return doReport(ctx, st, o, o.runID, nil)
	case "all":
		res, err := doIngest(ctx, st, o)
		if err != nil {
			return err
		}
		return doReport(ctx, st, o, res.runID, res)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

type ingestResult struct {
	runID       int64
	ing         *ingest.Ingester
	payRows     int
	setRows     int
	scopeNotes  []string
}

func doIngest(ctx context.Context, st *store.Store, o options) (*ingestResult, error) {
	start := time.Now()

	fmt.Println("==> applying schema")
	if err := st.ApplySchema(ctx, o.schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	fmt.Println("==> loading mapping configs into the database")
	payRules, err := ingest.LoadPaymentConfig(o.paymentConfig)
	if err != nil {
		return nil, err
	}
	setRules, err := ingest.LoadSettlementConfig(o.settlementConfig)
	if err != nil {
		return nil, err
	}
	if err := st.LoadPaymentConfig(ctx, payRules); err != nil {
		return nil, err
	}
	if err := st.LoadSettlementConfig(ctx, setRules); err != nil {
		return nil, err
	}
	fmt.Printf("    payment_config: %d rules   settlement_config: %d rules\n", len(payRules), len(setRules))

	if o.fixes != "" {
		fmt.Printf("==> applying mapping fixes from %s\n", o.fixes)
		if err := st.ApplyFile(ctx, o.fixes); err != nil {
			return nil, fmt.Errorf("apply fixes: %w", err)
		}
	}

	// Read the config back out of the database: the pipeline always runs off
	// stored config, so a fix is a data change, never a code change.
	payRules, err = st.ReadPaymentConfig(ctx)
	if err != nil {
		return nil, err
	}
	setRules, err = st.ReadSettlementConfig(ctx)
	if err != nil {
		return nil, err
	}

	paySHA, err := ingest.SHA256(o.payments)
	if err != nil {
		return nil, err
	}
	setSHA, err := ingest.SHA256(o.settlements)
	if err != nil {
		return nil, err
	}
	runID, err := st.StartRun(ctx, filepath.Base(o.payments), paySHA, filepath.Base(o.settlements), setSHA, o.revision)
	if err != nil {
		return nil, err
	}
	fmt.Printf("==> ingest run %d (%s)\n", runID, o.revision)

	ing := ingest.New(payRules, setRules)
	w := st.NewLedgerWriter(ctx, runID, 5000)

	payRows, setRows, err := ing.Run(o.payments, o.settlements, w.Add)
	if err != nil {
		return nil, err
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}

	// Persist the summary accumulated *during* ingestion.
	buckets := map[model.Source]map[string]struct {
		Amount decimal.Decimal
		Count  int
	}{}
	for src, m := range ing.Summary {
		buckets[src] = map[string]struct {
			Amount decimal.Decimal
			Count  int
		}{}
		for f, b := range m {
			buckets[src][f] = struct {
				Amount decimal.Decimal
				Count  int
			}{b.Amount, b.Count}
		}
	}
	if err := st.WriteSummary(ctx, runID, buckets); err != nil {
		return nil, err
	}

	notes := fmt.Sprintf("payments=%d rows settlements=%d rows ledger=%d entries in %s",
		payRows, setRows, w.Total, time.Since(start).Round(time.Millisecond))
	if err := st.FinishRun(ctx, runID, payRows, setRows, notes); err != nil {
		return nil, err
	}
	fmt.Printf("    payments rows: %d   settlement rows: %d   ledger entries: %d   (%s)\n",
		payRows, setRows, w.Total, time.Since(start).Round(time.Millisecond))

	for _, d := range ing.Diagnostics {
		fmt.Println("    ! " + d.String())
	}
	for _, u := range ing.UnmappedList() {
		fmt.Printf("    ! UNMAPPED %s %s/%s/%s/%s  n=%d  amount=%s (first at line %d)\n",
			u.Source, u.TransactionType, u.AmountType, u.Description, u.AmountField,
			u.Count, u.Amount.StringFixed(2), u.FirstSourceLine)
	}

	return &ingestResult{runID: runID, ing: ing, payRows: payRows, setRows: setRows}, nil
}

func doReport(ctx context.Context, st *store.Store, o options, runID int64, ir *ingestResult) error {
	fmt.Println("==> reconciling")
	res, err := recon.Load(ctx, st.Pool(), runID)
	if err != nil {
		return err
	}
	fmt.Printf("    reconciled: %d   unreconciled payments: %d   unreconciled settlements: %d\n",
		res.CountReconciled, res.CountUnrecPayment, res.CountUnrecSettlement)

	paySum, setSum, err := readSummary(ctx, st, runID)
	if err != nil {
		return err
	}

	in := report.Input{
		SettlementID:      "",
		PaymentSummary:    paySum,
		SettlementSummary: setSum,
		Recon:             res,
	}

	if ir != nil && len(ir.ing.Settlements) > 0 {
		s := ir.ing.Settlements[0]
		in.SettlementID = s.ID
		in.SettlementPeriod = fmt.Sprintf("%s to %s", s.StartDate.Format("2006-01-02"), s.EndDate.Format("2006-01-02"))
		in.DepositDate = s.DepositDate.Format("2006-01-02")
		in.Currency = s.Currency
		in.AmazonTotal = s.TotalAmount
	} else if err := loadSettlementHeader(ctx, st, runID, &in); err != nil {
		return err
	}

	if ir != nil {
		for _, d := range ir.ing.Diagnostics {
			in.ConfigDiagnostics = append(in.ConfigDiagnostics, d.String())
		}
		for _, u := range ir.ing.UnmappedList() {
			in.UnmappedRows = append(in.UnmappedRows, fmt.Sprintf(
				"%s  %s / %s / %s / %s   rows=%d  amount=%s  (first at source line %d)",
				u.Source, u.TransactionType, u.AmountType, u.Description, u.AmountField,
				u.Count, u.Amount.StringFixed(2), u.FirstSourceLine))
		}
	}
	in.ScopeNotes, err = scopeNotes(ctx, st, runID)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(o.out), 0o755); err != nil {
		return err
	}
	if err := report.Write(o.out, in); err != nil {
		return err
	}
	fmt.Printf("==> wrote %s\n", o.out)

	printSummary(in)
	return nil
}

func readSummary(ctx context.Context, st *store.Store, runID int64) (map[string]decimal.Decimal, map[string]decimal.Decimal, error) {
	rows, err := st.Pool().Query(ctx,
		`SELECT source, summary_field, amount FROM summary_bucket WHERE run_id = $1`, runID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	pay := map[string]decimal.Decimal{}
	set := map[string]decimal.Decimal{}
	for rows.Next() {
		var src, field string
		var amt decimal.Decimal
		if err := rows.Scan(&src, &field, &amt); err != nil {
			return nil, nil, err
		}
		if src == "PAYMENT" {
			pay[field] = amt
		} else {
			set[field] = amt
		}
	}
	return pay, set, rows.Err()
}

func loadSettlementHeader(ctx context.Context, st *store.Store, runID int64, in *report.Input) error {
	var id, start, end, dep, total, cur string
	err := st.Pool().QueryRow(ctx, `
		SELECT raw_payload->>'settlement-id', raw_payload->>'settlement-start-date',
		       raw_payload->>'settlement-end-date', raw_payload->>'deposit-date',
		       raw_payload->>'total-amount', raw_payload->>'currency'
		  FROM ledger_entry
		 WHERE run_id=$1 AND source='SETTLEMENT' AND raw_payload->>'total-amount' <> ''
		 LIMIT 1`, runID).Scan(&id, &start, &end, &dep, &total, &cur)
	if err != nil {
		return nil // header row is optional
	}
	in.SettlementID, in.SettlementPeriod, in.DepositDate, in.Currency = id, start+" to "+end, dep, cur
	in.AmazonTotal, _ = decimal.NewFromString(total)
	return nil
}

func scopeNotes(ctx context.Context, st *store.Store, runID int64) ([]string, error) {
	rows, err := st.Pool().Query(ctx, `
		SELECT source, settlement_id, COALESCE(NULLIF(transaction_status,''),'(n/a)'), in_scope,
		       COUNT(*)::int,
		       SUM(CASE WHEN source='PAYMENT' AND amount_field='TOTAL' THEN amount
		                WHEN source='SETTLEMENT' THEN amount ELSE 0 END)::numeric
		  FROM ledger_entry
		 WHERE run_id=$1 AND route_seq=0
		 GROUP BY 1,2,3,4 ORDER BY 1,2,3`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var src, sid, status string
		var inScope bool
		var n int
		var amt decimal.Decimal
		if err := rows.Scan(&src, &sid, &status, &inScope, &n, &amt); err != nil {
			return nil, err
		}
		flag := "EXCLUDED"
		if inScope {
			flag = "in scope"
		}
		out = append(out, fmt.Sprintf("%-10s settlement %-14s %-9s %-9s rows=%-6d total=%s",
			src, sid, status, flag, n, amt.StringFixed(2)))
	}
	return out, rows.Err()
}

func printSummary(in report.Input) {
	fmt.Println()
	fmt.Printf("%-34s %16s %16s %16s\n", "Summary line", "Payments", "Settlements", "Difference")
	fmt.Println(strings.Repeat("-", 86))
	var pt, stt decimal.Decimal
	for _, l := range summary.Lines {
		if l.Field == "" {
			fmt.Printf("%-34s\n", l.Label)
			continue
		}
		p, s := in.PaymentSummary[l.Field], in.SettlementSummary[l.Field]
		d := p.Sub(s)
		pt, stt = pt.Add(p), stt.Add(s)
		marker := "  "
		if !d.IsZero() {
			marker = "<<"
		}
		fmt.Printf("  %-32s %16s %16s %16s %s\n", l.Label, p.StringFixed(2), s.StringFixed(2), d.StringFixed(2), marker)
	}
	fmt.Println(strings.Repeat("-", 86))
	fmt.Printf("%-34s %16s %16s %16s\n", "TOTAL", pt.StringFixed(2), stt.StringFixed(2), pt.Sub(stt).StringFixed(2))
	fmt.Printf("%-34s %16s\n", "Amazon settlement total", in.AmazonTotal.StringFixed(2))
	fmt.Printf("%-34s %16s %16s\n", "Variance vs Amazon",
		pt.Sub(in.AmazonTotal).StringFixed(2), stt.Sub(in.AmazonTotal).StringFixed(2))

	// Any bucket that carries money but has no Summary line is a defect signal.
	var orphans []string
	seen := map[string]bool{}
	for f := range in.PaymentSummary {
		seen[f] = true
	}
	for f := range in.SettlementSummary {
		seen[f] = true
	}
	for f := range seen {
		if _, ok := summary.FieldToLine[f]; ok {
			continue
		}
		p, s := in.PaymentSummary[f], in.SettlementSummary[f]
		if p.IsZero() && s.IsZero() {
			continue
		}
		note := ""
		if _, known := summary.Unrouted[f]; !known {
			note = "   <-- no Summary line for this field"
		}
		orphans = append(orphans, fmt.Sprintf("  %-32s %16s %16s%s", f, p.StringFixed(2), s.StringFixed(2), note))
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		fmt.Println("\nSummary fields with no Summary line:")
		for _, o := range orphans {
			fmt.Println(o)
		}
	}
	fmt.Println()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var _ = mapping.Wildcard
