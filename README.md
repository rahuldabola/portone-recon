# Amazon Payments vs Settlement Reconciliation

Ingests Amazon's Payments report and Amazon's Settlement report for the same
settlement period into PostgreSQL, maps both through config-driven rules,
reconciles them on a shared `record_ref`, and produces an accounting workbook
whose two columns are derived independently and tie to Amazon's own number.

**Result for settlement `12395580393` (17 Jul – 31 Jul 2026, AUD):**

| | |
|---|---|
| Amazon's reported settlement total | **212,118.95** |
| Summary — Payments column | **212,118.95** (variance **0.00**) |
| Summary — Settlements column | **212,118.95** (variance **0.00**) |
| Every Summary line, Payments − Settlements | **0.00** |
| Reconciled `record_ref`s | **13,289** |
| Unreconciled — payment only | **26** (all explained, see below) |
| Unreconciled — settlement only | **0** |

The 26 unreconciled payments are the bank disbursement
(`Transfer / To account ending with: 334`, −133,756.51 — real money, but not a
settlement *component*, and correctly not summarised) plus 25 zero-value order
lines that never reached the settlement. Nothing else is outstanding.

---

## Running it end to end

Prerequisites: Go 1.22+, PostgreSQL 14+.

```bash
createdb portone_recon           # or: psql -c 'CREATE DATABASE portone_recon;'
export RECON_DSN='postgres://postgres:postgres@127.0.0.1:5432/portone_recon'

go build -o out/recon ./cmd/recon

# 1. before the fixes — reproduces the defective configs as delivered
./out/recon all -revision before-fix -out out/report_before_fix.xlsx

# 2. after the fixes — applies MAPPING_FIXES.sql to the config tables
./out/recon all -fixes MAPPING_FIXES.sql -revision after-fix -out out/report_after_fix.xlsx
```

`all` = ingest → reconcile → report. The two halves can also be run separately
(`recon ingest …` then `recon report -run <id> …`).

Flags: `-dsn -schema -payments -settlements -payment-config -settlement-config
-fixes -out -revision -run`. Defaults point at `data/` and `sql/001_schema.sql`.

Each run drops and rebuilds the schema, so re-running is idempotent — the same
inputs always produce the same database and the same workbook. `ingest_run`
records the SHA-256 of both input files so a run can be tied back to exactly
what it read. Full ingest of 78,006 source rows → 308,265 ledger rows takes
~37 s.

### What's in the repo

```
cmd/recon/            CLI: ingest / report / all
internal/normalize/   text + date canonicalisation (incl. the GMT offset parsing)
internal/mapping/     config matching, record_ref templating, config linter
internal/ingest/      file parsers, fan-out, streaming summary
internal/recon/       record_ref aggregation + matching
internal/report/      xlsx writer
internal/store/       schema, bulk load, reconciliation queries
sql/001_schema.sql    DDL
MAPPING_FIXES.sql     the defect fixes, one commented block each
PROGRESS.md           how each variance was isolated (and what I ruled out)
out/                  generated reports + pg_dump
```

---

## Schema design

Everything lives in five tables. The DDL is `sql/001_schema.sql`.

### `ledger_entry` — both files, one table

The assignment requires a single table for both sources. The problem is that the
two files have different shapes: a settlement row is one amount component
(`amount_type` / `amount_description` / `amount`), while a payments row spreads
money across eleven typed columns.

So payments rows **fan out at ingest**: one `ledger_entry` per populated money
column. That is exactly the grain the payment config addresses through its
`amount_field` column, and it makes the two sources genuinely the same shape —
one row per amount component — rather than a union of two different things
sharing a table. 23,026 payments rows + 54,980 settlement rows → 308,265 ledger
rows.

Columns fall into five groups:

- **provenance** — `source`, `source_file`, `source_line`, `raw_payload jsonb`.
  Every row keeps its complete source record verbatim, so any number in the
  report resolves to the exact input lines behind it. The Consolidated sheet
  carries those line numbers.
- **identity** — `settlement_id`, `txn_ref`, `sku`, `shipment_id`,
  `merchant_order_id`, `adjustment_id`, `quantity`.
- **classification** — normalised `transaction_type`, `description`,
  `amount_type`, `amount_description`, `amount_field`, plus the raw spellings
  kept alongside for the audit columns.
- **money & time** — `amount numeric(18,4)`, `posted_at`, `released_at`,
  `event_date`. Money is `numeric` end to end and `shopspring/decimal` in Go —
  no float arithmetic anywhere in the money path.
- **resolution** — `record_ref`, `summary_field`, `config_id`, `match_kind`,
  `route_seq`, `in_scope`.

Two of those deserve a note:

`route_seq` — a single amount can be matched by more than one config rule. That
is a defect (it double-counts), but the engine reproduces it rather than
silently picking one: the first match is `route_seq = 0`, extras are
`route_seq > 0`. So `route_seq = 0` is the true set of amount components, and
the duplicates stay visible and countable. Both before-fix duplicate routings
were found this way.

`in_scope` — see "Scope" below. It is stored, not applied as a filter at read
time, so excluded rows are still ingested and still auditable.

### `payment_config` / `settlement_config`

The mapping rules live in tables, which is what makes the step-5 fixes
`UPDATE`/`INSERT`/`DELETE` statements rather than code changes. The pipeline
loads the CSVs into these tables, optionally applies `MAPPING_FIXES.sql`, then
**re-reads the config out of the database** and ingests from that. There is no
path by which a fix can be anything other than a data change.

### `summary_bucket` — computed during ingest

The accounting summary is accumulated **as rows stream past**, not aggregated
afterwards. `Ingester.resolve` adds each in-scope, non-zero amount to its bucket
at the moment it resolves the rule, and the totals are written once at the end
of the run. There is no `GROUP BY` over `ledger_entry` anywhere in the summary
path. (The *reconciliation* does query the ledger — that is a different job,
and it needs the record_ref-level detail the Consolidated sheet prints.)

### `ingest_run`

One row per run, with both input SHA-256s and a `config_revision` label
(`before-fix` / `after-fix`). Every ledger and summary row is `ON DELETE
CASCADE` from it.

---

## How the mapping is applied

### Normalisation

Config keys are upper-case with underscores for spaces; the source files are
not consistent. `normalize.Key` upper-cases and collapses whitespace runs to a
single `_`. Hyphens, colons, parentheses and ampersands are significant and
preserved, because the config keys contain them:

```
payments   "Service fee"                 -> SERVICE_FEE
payments   "FBA Inventory Reimbursement - Damaged:Warehouse"
                                         -> FBA_INVENTORY_REIMBURSEMENT_-_DAMAGED:WAREHOUSE
settlement "other-transaction"           -> OTHER-TRANSACTION
settlement "Base fee"                    -> BASE_FEE
settlement "FBA Inventory Reimbursement" -> FBA_INVENTORY_REIMBURSEMENT
```

### Rule selection

Within a transaction type, description match precedence is **exact → longest
prefix → `any` wildcard**. The prefix tier is needed for rules like
`TRANSFER / TO_ACCOUNT_ENDING`, which has to match
`TO_ACCOUNT_ENDING_WITH:_334`, and `SAFE-T_REIMBURSEMENT / SAFE-T_CLAIM_ID`.

Catch-all rules (empty `transaction_type`) are reached **only when the
transaction type is entirely unknown to the config** — not when a known type
simply has no rule for one amount column.

> **This is a fix in the matching logic, not in the config, and it matters.**
> The payments config has catch-all rules keyed on amount field
> (`,any,total`, `,any,other`, `,any,other_transaction_fees`) whose record_ref
> template is `txn_ref+settlement_id+date`. Falling back per-column gave every
> ORDER row a *second*, differently-shaped record_ref for its empty `other`
> column — 12,034 phantom "unreconciled payment" keys carrying no money. Scoping
> the fallback to the row level took unreconciled payments from 12,034 to 26.
> This could not be fixed in the config: the catch-all rules are correct, the
> interpretation of them was not.

Amount columns with no rule under a known type are ingested with
`match_kind = 'UNMAPPED'` and an empty `record_ref`; they are excluded from
matching but reported on the Summary sheet if they carry any money.

### `record_ref`

`mapping.BuildRecordRef` splits the `+`-joined template and substitutes field
tokens: `txn_ref`, `sku`, `date`, `settlement_id`, `shipment_id`,
`merchant_order_id`, `adjustment_id`, `description`, `record_type`. Everything
else is a literal and passes through untouched.

Tokens are matched **case-sensitively against lower-case names**. That is not
incidental: every field token in both configs is lower-case while every literal
is upper-case (`ADJUSTMENT`, `LOST:WAREHOUSE`, `GENERAL ADJUSTMENT`,
`FBA_DISPOSAL_FEE`), so a literal can never be mistaken for a field. The
rendered key is upper-cased so the two sides cannot drift apart on case alone.

### The `date` token

The single most important normalisation in the pipeline, and the one that is
not obvious from the files.

Payments rows carry both a posted `date/time` and a `Transaction Release Date`,
in local time with an explicit offset (`GMT+9` in this export — *not* AEST, so
the offset is read from the string, never assumed). Settlement rows carry
`posted-date` in UTC.

The settlement report's `posted-date` is the **release** date, not the order's
posted date. Measured over every Order row against the settlement's 13,217
distinct `(order, sku, posted-date)` keys:

| payments date used | hits | misses |
|---|---:|---:|
| `date/time`, UTC | 0 | 22,783 |
| `date/time`, local | 0 | 22,783 |
| **`Transaction Release Date` → UTC** | **13,328** | 6,935 |
| `Transaction Release Date`, local | 8,145 | 12,118 |
| *(order + sku only, date ignored)* | *13,329* | *9,454* |

Release-date-in-UTC costs exactly one row against ignoring the date entirely, so
it is the right token rather than a lucky fit. Deferred rows have no release
date, carry no `event_date`, and therefore cannot join this settlement — which
is correct, and is the same fact the scope rule below rests on.

### Sign-based routing

`to_summary_field_when_positive_amount` / `…_when_negative_amount` are selected
per amount. Zero is treated as positive (it contributes nothing either way, but
keeps bucket counts deterministic). An empty target means the amount is
ingested but not summarised.

---

## Reconciliation

Both sides are **aggregated to `record_ref` first**, then matched set-to-set.
Never row-to-row.

That is forced by the data. A single payments Order line faces
ItemPrice/Principal + ItemPrice/Shipping + ItemFees/Commission +
ItemFees/FBAPerUnitFulfillmentFee + Promotion/Shipping on the settlement side.
In the other direction, order `249-4973033-3693468` has one reimbursement per
payments row and three settlement rows on 19.07. `record_ref` is designed for
exactly this — it is the coarsest key at which the two reports agree — so
aggregating to it before matching is the only join that does not need
row-level heuristics.

A worked example of the many-to-many, order `503-2302340-3488631` /
`BDPRM004QAFE1A`:

```
payments  24 Jul 18:45 GMT+9 -> 24.07 UTC   18.68
          24 Jul 23:31 GMT+9 -> 24.07 UTC   15.52
          25 Jul 04:37 GMT+9 -> 24.07 UTC   15.52
          25 Jul 07:09 GMT+9 -> 24.07 UTC   15.52   = 65.24 on 24.07
          25 Jul 09:49 GMT+9 -> 25.07 UTC   15.52   = 15.52 on 25.07
settlement                      24.07       18.68 + 15.52 + 15.52 + 15.52 = 65.24
                                25.07       15.52
```

Both the day split and the totals agree once the timezone is applied.

Classification: present on both sides → reconciled; payments only → unreconciled
payment; settlement only → unreconciled settlement. The Consolidated sheet
emits them in that order.

---

## Scope: which rows the report covers

The settlement file contains exactly one settlement, `12395580393`. The payments
export spans four settlement IDs and a wider date range, so a scope rule is
required.

**Scope = settlement `12395580393`, `Transaction status = Released`.**

Not an assumption — it is the only scope that reproduces Amazon's number:

| | rows | sum(total) |
|---|---:|---:|
| payments, settlement 12395580393, Released, order in settlement | 13,442 | **212,093.45** |
| settlement file, order rows | — | **212,093.45** |
| settlement file, non-order rows (blank order-id) | 4 | 25.50 |
| **settlement header `total-amount`** | | **212,118.95** |

Excluded, and printed on the Summary sheet rather than dropped silently:

- **2,392 Deferred rows, 43,540.07.** They carry this settlement's ID but no
  release date — Amazon flagging that they settle in a *later* period.
  Including them would overstate the settlement by that amount.
- **6,983 rows across three other settlement IDs.** Different periods.
- 173 rows whose order appears in this settlement but whose payments lines
  settled elsewhere. They do not collide, because matching is on
  `(order, sku, date)` rather than on order alone — and the scoped total landing
  exactly on 212,093.45 is the proof that they should not be pulled in.

`in_scope` is stored on every ledger row, so excluded rows remain queryable.

---

## The mapping defects

Eleven fixed in `MAPPING_FIXES.sql`, five documented as findings with what would
be needed to close them. Each has its own commented block there with the before
value, the after value and the reasoning. Summarised:

| # | Side | Defect | Effect on this settlement |
|---|---|---|---|
| 1 | payments | `ORDER/sales_tax_collected` routed to **two** buckets | 13,675.59 double counted |
| 2 | payments | `ORDER/low_value_goods` routed to **two** buckets | −196.62 double counted |
| 3 | settlement | order tax scattered across Product Charges / Shipping / Other | reclassified 13,478.97 to **Tax** |
| 4 | payments | gift wrap → `sales_gift_wrap_credits`, a bucket with no Summary line | 11.61 lost |
| 5 | payments | promotional rebates → Expenses>Other instead of Promo rebates | 11,273.53 misclassified |
| 6 | payments | refund tax not summarised at all | 42.59 missing |
| 7 | settlement | `REFUND/ITEMPRICE/TAX` positive branch → non-existent bucket | 0.00 (latent) |
| 8 | both | **FBA fulfilment fees reported as Amazon fees** | 96,803.17 reclassified, tie-neutral |
| 9 | payments | no rule for the TRANSFER `other` column | 0.00 (was right by accident) |
| 10 | payments | `AWD_STORAGE_FEES` vs `AWD_STORAGE_FEE` — keys can never join | 0.00 (latent) |
| 11 | settlement | malformed template `…+date+record_type+settlement_id` | 0.00 (latent) |

### The central one: tax (defects 1, 2, 3)

The two reports describe tax at incompatible granularity. Derived by reconciling
column against component over the 13,217 matched order groups:

```
payments `sales tax collected`  13,675.59
    = ITEMPRICE/TAX 13,765.71 + SHIPPINGTAX 581.68 + GIFTWRAPTAX 0.36
      + PROMOTION/TAXDISCOUNT -672.16

payments `low value goods`        -196.62
    = ITEMWITHHELDTAX/LOWVALUEGOODSTAX-PRINCIPAL -193.90 + -SHIPPING -2.72
```

The payments file has **two** tax columns; the settlement file has **six** tax
components. No routing that splits tax across Product Charges / Shipping /
Other can be reproduced from the payments side, because the payments side does
not carry the split. Deleting one duplicate rule and keeping the other does not
work either — the arithmetic cannot tie in either direction.

The Summary sheet has a **"Tax"** line that was unreachable: the only rules
pointing at `sales_tax` are COMMINGLING_VAT, which does not occur in AU.
Collapsing all tax onto that one line on both sides is the only self-consistent
answer, and it reconciles to the cent:

```
payments   13,675.59 + (-196.62)                              = 13,478.97
settlement 13,765.71 + 581.68 + 0.36 - 672.16 - 193.90 - 2.72 = 13,478.97
```

### The one that agrees on both sides and is still wrong (defect 8)

> *"A Summary sheet whose two columns agree is necessary, not sufficient."*

Both configs route `FBAPerUnitFulfillmentFee` to `expenses_amazon_fees`, so it
produces **no variance** and the diff can never find it. It is still the largest
misclassification in the report: before the fix, Expenses > FBA fees read
−0.41 for a seller with 13,328 FBA shipments, while Expenses > Amazon fees held
−132,593.41.

Every other FBA fee in both configs — storage, long-term storage, removal,
disposal, return, AWD storage/processing/transportation, inbound placement,
customer returns — routes to `expenses_fba_fees`. The per-unit fulfilment fee,
the largest of them, is the only one that does not.

It is safe to move because the payments column maps exactly onto three
settlement components (−95,592.26 + −11.97 + −1,198.94 = −96,803.17), so both
sides move together and the tie-out is unchanged.

**Stated plainly:** gift wrap and shipping chargebacks are *not* FBA fees, but
the payments report merges them into the same column, so they cannot be
separated from the payments side. They are 1,210.91 of the 96,803.17 (1.3%).
This is a judgement call, it is tie-neutral, and it is isolated in its own block
in `MAPPING_FIXES.sql` so it can be reverted alone.

### Found by comparing templates, not by comparing numbers

Extracting every `record_ref` template from both configs and diffing the sets
surfaces defects the Summary can never show, because a template used on only
one side produces keys that can never join. Eight payments-only and thirteen
settlement-only templates; most are legitimately one-sided (the bank transfer,
the reserve balances), four are not — defects 10 and 11 above, and findings A
and B in `MAPPING_FIXES.sql`.

### Not fixed, and why

Five findings are documented at the bottom of `MAPPING_FIXES.sql` with no
statements attached: the failed-disbursement row with an empty template and the
unreachable "Micro Deposit (Failed)" line; the two shipment-level fees keyed on
`txn_ref` on one side and `shipment_id` on the other; the DEALS promotion fee
keys; the settlement-only balance fields; and
`total_refund_expense_or_sales_amt` / `total_adjustment_other_buyer_recharge_amt`,
which both configs use *symmetrically* — so they would hide money while the two
columns still agreed.

None occurs in this settlement, so each is 0.00 here. Each block states what
would be required to close it. Guessing at them would be exactly the plugging
the brief rules out.

---

## Auditability

Any figure in the report resolves to source rows:

- **Summary → line** — each line names its config `summary_field`.
- **Consolidated sheet** — 116 columns: the sample's 105 (payment identity +
  per-bucket amounts, settlement identity + per-bucket amounts, reconciliation
  status, per-bucket `DS1-DS2` differences) plus 11 audit columns:
  `record_ref`, row counts per side, source file and **source line numbers**
  per side, matched summary field per side, and `match_kind` per side.
- **Database** — `ledger_entry.raw_payload` holds the complete source row, and
  `(source_file, source_line)` pins it to the file.

```sql
-- every source row behind one Summary line
SELECT source, source_file, source_line, amount, record_ref, raw_payload
  FROM ledger_entry
 WHERE run_id = 1 AND in_scope AND summary_field = 'sales_tax'
 ORDER BY source, source_line;
```

The Summary sheet also carries, below the line items: the tie-out to Amazon,
reconciliation counts, any summary field carrying money with no Summary line,
the scope breakdown with excluded buckets and their values, config diagnostics
from the linter, and any rows that matched no config rule.

---

## Assumptions

1. **Scope is settlement `12395580393`, Released only.** Justified by the
   212,093.45 tie above rather than assumed. Deferred rows and the other three
   settlement IDs are ingested, marked out of scope, and reported.
2. **`date` = payments release date in UTC ↔ settlement `posted-date`.**
   Established by measurement (table above), not assumed.
3. **The settlement config is authoritative on classification.** The payments
   config is delivered as `…_au_old.csv`, and the settlement side already tied
   to Amazon's total before any change. Where the two disagreed on a pure
   classification question, payments was moved to match — except for tax, where
   the payments file cannot express the settlement's split (defect 3), and
   defect 8, where both moved together.
4. **`total` is never summarised.** Both configs leave `total` unrouted
   wherever a typed column carries the same money; summarising it would double
   count. Payments row totals are still used for the Consolidated sheet's
   `Total` column and for the scope arithmetic.
5. **The bank Transfer is not a settlement component.** It appears as an
   unreconciled payment and contributes nothing to the Summary — it is the
   disbursement of the settlement, not a line within it.
6. **Zero-amount columns still produce ledger rows** so a payments row is fully
   represented, but contribute nothing to any bucket.
7. **Tax on Low Value Goods belongs on the Tax line.** It is withheld tax. It
   could defensibly sit on Product Charges instead; what is *not* defensible is
   splitting it, since the payments side has one column. The alternative would
   also tie — this one is a judgement call, flagged as such in defect 2.

---

## Testing

```bash
go test ./...
```

Unit tests cover the pieces where a silent error would be a financial error:
timestamp parsing across both formats and the GMT-offset conversion that the
whole `date` token depends on, key normalisation, `record_ref` templating
(literal vs field, missing values), rule-match precedence including the
prefix tier and the row-level fallback, sign-based routing, and the config
linter's duplicate-route detection.

Also verified end to end: the two Summary columns are computed from disjoint
row sets (`source = 'PAYMENT'` / `source = 'SETTLEMENT'`) and never share an
intermediate, so their agreement is a real check rather than an artefact.

---

## Deliverables

| | |
|---|---|
| `out/report_before_fix.xlsx` | configs as delivered — Payments column off by **+13,509.95** |
| `out/report_after_fix.xlsx` | after `MAPPING_FIXES.sql` — **every line 0.00**, both columns tie to 212,118.95 |
| `MAPPING_FIXES.sql` | 11 fixes + 5 documented findings, one commented block each |
| `out/portone_recon_after_fix.sql` | `pg_dump` of the post-ingest database |
| `PROGRESS.md` | how each variance was isolated, and what I ruled out |
