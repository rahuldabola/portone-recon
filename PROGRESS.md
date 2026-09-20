# Progress log

A running note of how this was actually built and, more importantly, how each
variance was isolated. Things I tried and discarded are kept in, because the
discards are where most of the understanding came from.

---

## 1. Reading the inputs before writing anything

Profiled both files first rather than guessing at a schema.

**Payments CSV** — 23,026 data rows behind a 9-line free-text preamble; the real
header is the row starting `date/time`. Money is spread across 11 typed columns.

| dimension | values |
|---|---|
| `type` | Order 22,783 · Refund 139 · Adjustment 97 · Service fee 3 · Transfer 2 · FBA txn fees 2 |
| `Transaction status` | Released 20,498 · Deferred 2,528 |
| `settlement ID` | 12395580393 (15,870) · 12382593803 (5,649) · 12407469483 (1,493) · 12370691583 (14) |

**Settlement TXT** — 54,980 tab-separated rows, all for **one** settlement,
`12395580393`, period 17.07.2026 → 31.07.2026, deposit 02.08.2026.

First useful fact: the `amount` column sums to **212,118.95**, exactly the
`total-amount` on the file's header row. So the settlement file is internally
consistent and gives an external anchor to tie the whole report back to.

Second: the payments file's grand total over *all* rows is 150,209.15 — nowhere
near 212,118.95. So the two files do not correspond row-for-row or even
file-for-file, and the scope question had to be settled before anything else.

---

## 2. The `date` token — the first real problem

Both configs build `record_ref` from a `date` token, but the two files carry
several dates and they disagree:

- payments row: order `503-8856864-4518217`, `date/time` = **29 June 2026**
- the same order in settlements: `posted-date` = **17.07.2026**

Amounts matched exactly (24.59 / 3.00 / -6.17 / -3.52 / -3.00), so it was
plainly the same money with a different date. The payments row's
`Transaction Release Date` was *18 July 2026 6:10:51 am GMT+9* — which in UTC is
**17 July 21:10**, i.e. `17.07.2026`.

Tested all four candidates over every Order row against the 13,217 distinct
settlement `(order, sku, posted-date)` keys:

| payments date used | hits | misses |
|---|---:|---:|
| `date/time`, UTC | 0 | 22,783 |
| `date/time`, local | 0 | 22,783 |
| **`Transaction Release Date`, UTC** | **13,328** | 6,935 |
| `Transaction Release Date`, local | 8,145 | 12,118 |
| order+sku only, date ignored | 13,329 | 9,454 |

Release-date-in-UTC scores 13,328 against 13,329 for ignoring the date entirely
— it costs exactly one row, so it is the right token and not a lucky fit.
Also note the offset in the file is `GMT+9`, which is *not* AEST; the parser
reads the offset from the string rather than assuming a marketplace timezone.

Deferred rows have no release date at all, which turned out to matter next.

---

## 3. Settling the scope — and finding the tie-out

Broke the payments file down by settlement id × status × "is this order in the
settlement file":

| in settlement | settlement ID | status | rows | sum(total) |
|---|---|---|---:|---:|
| yes | 12395580393 | Released | 13,442 | **212,093.45** |
| yes | 12395580393 | Deferred | 3 | 59.98 |
| yes | 12382593803 | Released | 165 | 2,620.90 |
| yes | 12407469483 | Released | 8 | 100.71 |
| no | 12395580393 | Released | 30 | -133,731.01 |
| no | 12395580393 | Deferred | 2,395 | 43,480.09 |
| no | others | — | 6,983 | 25,585.03 |

`212,093.45` is exactly the settlement file's order-row total. The remaining
25.50 is its four non-order rows (three warehouse-damage reimbursements and a
subscription fee, all with a blank `order-id`), and 212,093.45 + 25.50 =
**212,118.95**.

So the scope is **settlement 12395580393, Released only**, and it reproduces
Amazon's own number to the cent. Recorded as `in_scope` on every ledger row, with
the excluded buckets printed on the Summary sheet so nothing is hidden:

- **2,392 Deferred rows (43,540.07)** — posted in the window but explicitly not
  released into this settlement; Amazon settles them later. Counting them would
  overstate the period.
- **6,983 rows on three other settlement IDs** — different periods.
- The 165 + 8 rows above are orders that *appear* in this settlement but whose
  payments lines settled elsewhere; they do not collide, because matching is on
  `(order, sku, date)`, not on order alone. That the scoped total lands exactly
  on 212,093.45 is the proof.

Worth noting the 30 in-scope rows whose order is not in the settlement file:
one Transfer (-133,756.51, the actual bank disbursement), three warehouse-damage
adjustments and a subscription fee that *do* match the settlement's blank-order
rows, and 25 zero-value orders.

---

## 4. The Rosetta Stone — deriving the correct routing instead of guessing

Rather than reason about what a rule "should" say, I aggregated both sides over
the 13,217 matched Order groups and compared column against component:

| payments column | amount | settlement components | amount |
|---|---:|---|---:|
| product_sales | 335,336.96 | ITEMPRICE/PRINCIPAL | 335,336.96 |
| shipping_credits | 8,294.16 | ITEMPRICE/SHIPPING | 8,294.16 |
| gift_wrap_credits | 11.61 | ITEMPRICE/GIFTWRAP | 11.61 |
| selling_fees | -35,773.27 | ITEMFEES/COMMISSION | -35,773.27 |
| fba_fees | -96,803.17 | FBAPERUNITFULFILLMENTFEE + GIFTWRAPCHARGEBACK + SHIPPINGCHARGEBACK | -96,803.17 |
| promotional_rebates | -11,273.53 | PROMOTION/PRINCIPAL + PROMOTION/SHIPPING | -11,273.53 |
| **sales_tax_collected** | **13,675.59** | ITEMPRICE/TAX + SHIPPINGTAX + GIFTWRAPTAX + PROMOTION/TAXDISCOUNT | **13,675.59** |
| **low_value_goods** | **-196.62** | LOWVALUEGOODSTAX-PRINCIPAL + -SHIPPING | **-196.62** |

Every payments column maps onto an exact set of settlement components. The same
exercise on the 71 Refund groups matched just as cleanly.

This is what made the rest of the work evidence-based: any routing where the two
sides send a matched pair to different buckets is a defect, and — crucially —
any settlement split that the payments side cannot express is *unfixable* by
changing the payments side alone.

---

## 5. First run, and a bug that was mine, not the config's

First end-to-end run reported **13,289 reconciled / 12,034 unreconciled payments
/ 0 unreconciled settlements**. 12,034 was obviously wrong — only ~13,400
payments rows are in scope.

Cause: my matcher fell back to the config's catch-all rules (empty
`transaction_type`) *per amount column*. An ORDER row's empty `other` column
found no `ORDER` rule, fell through to `,any,other`, and picked up that rule's
`txn_ref+settlement_id+date` template — minting a second, differently-shaped
record_ref for every order, carrying no money.

Fixed in the matching logic, not the config (and called out as such in the
README): the catch-all applies only when the transaction *type* is entirely
unknown to the config. Ledger rows that match no rule keep an empty record_ref
and are excluded from matching but still reported.

After the fix: **13,289 / 26 / 0**, and all 26 are explained — the bank Transfer
plus 25 zero-value orders.

Second, smaller find from the same run: the two Transfer rows carry the
disbursement in *both* `other` and `total`, and the config has no rule for
`other`, so it was logged UNMAPPED at -231,676.27. Harmless (unmapped
contributes nothing, which is correct for a disbursement) but accidental —
defect 9.

---

## 6. Before-fix variances, and tracing each one

Settlements column: **212,118.95, variance vs Amazon 0.00** — the settlement
config was already correct. Payments column: 225,628.90, **+13,509.95**.

| Summary line | Payments | Settlements | Diff |
|---|---:|---:|---:|
| Sales > Product Charges | 348,815.93 | 348,908.77 | -92.84 |
| Sales > Shipping | 21,773.13 | 8,200.96 | +13,572.17 |
| Sales > Other | 0.00 | 11.97 | -11.97 |
| Refunds > Refund expenses | 258.87 | 216.28 | +42.59 |
| Expenses > Promo rebates | 0.00 | -11,273.53 | +11,273.53 |
| Expenses > Other | -11,273.53 | 0.00 | -11,273.53 |
| *(orphan)* sales_gift_wrap_credits | 11.61 | 0.00 | — |

Sum of the differences = 13,509.95, so the variance was fully accounted for
before a single change was made.

**Shipping +13,572.17 and Product Charges -92.84** decomposed as:

```
payments Shipping        = shipping_credits 8,294.16
                         + sales_tax_collected 13,675.59   <- double counted
                         + low_value_goods -196.62         <- double counted
                         = 21,773.13
payments Product Charges = product_sales 335,336.96
                         + sales_tax_collected 13,675.59   <- double counted
                         + low_value_goods -196.62         <- double counted
                         = 348,815.93
```

Both tax columns had **two** config rules each, routing the same amount to two
buckets — the ingest linter had already flagged it:

```
! [PAYMENT] DUPLICATE_ROUTE: ORDER||ANY|LOW_VALUE_GOODS     ... lines [5 6]
! [PAYMENT] DUPLICATE_ROUTE: ORDER||ANY|SALES_TAX_COLLECTED ... lines [71 72]
```

The obvious repair — keep one rule each — does not work. Delete the
`sales_shipping` duplicates and the payments side puts *all* tax in Product
Charges, while the settlement side splits it (TAX → product charges,
SHIPPINGTAX → shipping, GIFTWRAPTAX → other, TAXDISCOUNT → shipping). Those two
can never agree, because the payments file has one tax column and the settlement
file has six tax components.

That is what pointed at the **"Tax" line**, which sits on the Summary sheet and
was unreachable: the only rules routing to `sales_tax` are COMMINGLING_VAT,
which does not occur in AU. Collapsing all tax to that one line on both sides is
the only self-consistent answer, and it checks out exactly:

```
payments   13,675.59 + (-196.62)                              = 13,478.97
settlement 13,765.71 + 581.68 + 0.36 - 672.16 - 193.90 - 2.72 = 13,478.97
```

**Refund expenses +42.59** — payments `REFUND / sales_tax_collected` had empty
targets (not summarised) while the settlement side books the same -42.59 to
`refunded_expenses`. What flagged it was the inconsistency with the ORDER side,
which *does* summarise its tax.

**Promo rebates / Other ±11,273.53** — a pure reclassification: payments sent
`promotional_rebates` to `expenses_other`, settlement sent the matching
PROMOTION components to `expenses_promotional_rebates`.

**Sales Other -11.97** — payments routed gift wrap to `sales_gift_wrap_credits`,
a bucket with no Summary line, so 11.61 vanished. The remaining 0.36 is
GIFTWRAPTAX, which defect 3 moves to Tax; the two together close it exactly.

---

## 7. Looking for defects that do *not* show as a variance

The brief warns that agreeing columns are necessary but not sufficient, so I
went looking specifically for defects invisible to the diff.

**Template comparison.** Extracted every `record_ref` template from both configs
and diffed the sets — a template used on only one side can never reconcile. 8
payments-only and 13 settlement-only templates; most are legitimately
one-sided (the bank Transfer, the reserve balances), but four are not:
`AWD_STORAGE_FEES` vs `AWD_STORAGE_FEE` (defect 10), the malformed
`ADJUSTMENT_OTHER+settlement_id+date+record_type+settlement_id` (defect 11), the
empty template on the failed-disbursement row (finding A), and the two
shipment-level fees keyed on `txn_ref` on one side and `shipment_id` on the
other (finding B).

**Orphan-bucket sweep.** The report lists any summary field carrying money with
no Summary line. `sales_gift_wrap_credits` showed up (defect 4);
`total_refund_expense_or_sales_amt` and
`total_adjustment_other_buyer_recharge_amt` are used *symmetrically* by both
configs, so they would hide money while the columns still agreed — documented as
finding E, with the one leak-capable case closed as defect 7.

**Line-item plausibility.** Expenses > FBA fees read -0.41 for a seller with
13,328 FBA shipments, while Expenses > Amazon fees held -132,593.41. Both
configs route `FBAPERUNITFULFILLMENTFEE` to `expenses_amazon_fees`, so it
produced no variance. Every other FBA fee in both configs — storage, long-term
storage, removal, disposal, return, AWD, inbound placement, customer returns —
goes to `expenses_fba_fees`. Because the payments `fulfilment by amazon fees`
column maps exactly onto three settlement components, both sides can be moved
together and the tie-out is unaffected. Applied as defect 8, flagged separately
as a judgement call and reversible on its own.

---

## 8. Things I ruled out

- **Matching on `(order, sku)` without a date.** One row better than the
  release-date key, but it would collapse records that legitimately settle on
  different days — order `503-2302340-3488631` has four reimbursement rows on
  24.07 and a fifth on 25.07, and both sides agree on that split.
- **Including Deferred rows.** Would add 43,540.07 to the Payments column with
  no settlement counterpart. They carry this settlement's ID but no release
  date, which is Amazon saying they belong to a later settlement.
- **Excluding the Transfer row from the report.** It stays in as an
  unreconciled payment: it is real money leaving the account, it just is not a
  settlement *component*. Its config rules correctly do not summarise it.
- **Splitting `fba_fees` so only FBAPERUNITFULFILLMENTFEE moves to FBA fees.**
  Would be better accounting but is not expressible: the payments report merges
  the per-unit fee with gift-wrap and shipping chargebacks into one column, so
  the split cannot be reproduced from the payments side. Took the tie-preserving
  option and documented the 1,210.91 (1.3%) that travels with it.
- **Deleting one duplicate tax rule and keeping the other.** See §6 — arithmetic
  says it cannot tie either way.

---

## 9. Where it landed

See README "Results". Both Summary columns agree on every line, and both tie to
Amazon's reported settlement total of 212,118.95 with zero variance. 13,289
reconciled record_refs, 26 unreconciled payments (all explained), 0 unreconciled
settlements.
