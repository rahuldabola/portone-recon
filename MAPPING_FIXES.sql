-- ===========================================================================
-- MAPPING_FIXES.sql
-- PortOne Prism — Amazon Payments vs Settlement reconciliation (AU)
--
-- Applied to the config TABLES, not to the CSVs and never to report code:
--
--     recon all -fixes MAPPING_FIXES.sql -revision after-fix
--
-- The pipeline loads both CSVs into payment_config / settlement_config, runs
-- this file, then re-reads the config out of the database and ingests. Every
-- fix below is therefore a data change.
--
-- Config values are stored normalised (upper-cased, whitespace -> '_'), which
-- is why the WHERE clauses read 'ANY', 'SALES_TAX_COLLECTED', 'ITEMPRICE'.
--
-- Where a defect is NOT fixed here, there is still a commented block for it,
-- stating what it is and what would be needed to close it.
--
-- Evidence for the "before" numbers: out/report_before_fix.xlsx, and
-- PROGRESS.md for how each one was isolated.
--
-- -------------------------------------------------------------------------
-- Which side is authoritative?
--
-- The payments config is delivered as `amazon_payment_configs_au_old.csv` and
-- the settlement config as `amazon_settlement_configs_au.csv`. The settlement
-- config is also the one that already ties exactly to Amazon's own settlement
-- total (212,118.95) before any change. So where the two disagree on a pure
-- classification question, the payments side is moved to match the settlement
-- side — unless the payments file is physically incapable of expressing the
-- settlement side's split, which is the case for tax (defect 3).
-- ===========================================================================

BEGIN;


-- ===========================================================================
-- DEFECT 1 — payments: sales tax is routed into two buckets at once
-- ---------------------------------------------------------------------------
-- WAS (two rows, config lines 71 and 72):
--   ORDER, any, sales_tax_collected, txn_ref+sku+date, sales_product_charges, sales_product_charges
--   ORDER, any, sales_tax_collected, txn_ref+sku+date, sales_shipping,        sales_shipping
--
-- Both rules match the same amount column of the same row, so every dollar of
-- `sales tax collected` was counted twice — once in Sales>Product Charges and
-- again in Sales>Shipping. 13,675.59 of tax, double counted.
--
-- NOW: one rule, routed to Sales>Tax. See defect 3 for why Tax is the only
-- bucket at which the two reports can agree.
--
-- Effect: Sales>Product Charges -13,675.59 ; Sales>Shipping -13,675.59 ;
--         Sales>Tax +13,675.59.
-- ===========================================================================

DELETE FROM payment_config
 WHERE transaction_type = 'ORDER' AND description = 'ANY'
   AND amount_field = 'SALES_TAX_COLLECTED'
   AND summary_field_positive = 'sales_shipping';

UPDATE payment_config
   SET summary_field_positive = 'sales_tax',
       summary_field_negative = 'sales_tax'
 WHERE transaction_type = 'ORDER' AND description = 'ANY'
   AND amount_field = 'SALES_TAX_COLLECTED';


-- ===========================================================================
-- DEFECT 2 — payments: low value goods tax is routed into two buckets at once
-- ---------------------------------------------------------------------------
-- WAS (two rows, config lines 5 and 6):
--   ORDER, any, low_value_goods, txn_ref+sku+date, sales_shipping,        sales_shipping
--   ORDER, any, low_value_goods, txn_ref+sku+date, sales_product_charges, sales_product_charges
--
-- Same defect as 1. The author was clearly trying to mirror the settlement
-- side, which DOES split this amount across two components
-- (LOWVALUEGOODSTAX-PRINCIPAL -> product charges, LOWVALUEGOODSTAX-SHIPPING ->
-- shipping). But the payments report exposes a single `low value goods`
-- column, so the split cannot be expressed there and the two rules just
-- double-count: -196.62 counted twice.
--
-- NOW: one rule, routed to Sales>Tax. Low Value Goods GST is tax withheld by
-- Amazon; it belongs on the Tax line with the rest of the tax.
--
-- Effect: Sales>Product Charges +196.62 ; Sales>Shipping +196.62 ;
--         Sales>Tax -196.62.
-- ===========================================================================

DELETE FROM payment_config
 WHERE transaction_type = 'ORDER' AND description = 'ANY'
   AND amount_field = 'LOW_VALUE_GOODS'
   AND summary_field_positive = 'sales_shipping';

UPDATE payment_config
   SET summary_field_positive = 'sales_tax',
       summary_field_negative = 'sales_tax'
 WHERE transaction_type = 'ORDER' AND description = 'ANY'
   AND amount_field = 'LOW_VALUE_GOODS';


-- ===========================================================================
-- DEFECT 3 — settlement: order tax is scattered across Product Charges,
--            Shipping and Other, so it can never agree with the payments side
-- ---------------------------------------------------------------------------
-- WAS:
--   ORDER, ITEMPRICE,       TAX,                        -> sales_product_charges
--   ORDER, ITEMPRICE,       SHIPPINGTAX,                -> sales_shipping
--   ORDER, ITEMPRICE,       GIFTWRAPTAX,                -> sales_other
--   ORDER, PROMOTION,       TAXDISCOUNT,                -> sales_shipping
--   ORDER, ITEMWITHHELDTAX, LOWVALUEGOODSTAX-PRINCIPAL, -> sales_product_charges
--   ORDER, ITEMWITHHELDTAX, LOWVALUEGOODSTAX-SHIPPING,  -> sales_shipping
--
-- Root cause, established by reconciling the two files column-by-column on the
-- 13,217 matched (order, sku, date) groups:
--
--   payments `sales tax collected` (13,675.59)
--       == ITEMPRICE/TAX      13,765.71
--        + ITEMPRICE/SHIPPINGTAX  581.68
--        + ITEMPRICE/GIFTWRAPTAX    0.36
--        + PROMOTION/TAXDISCOUNT -672.16      -> exactly 13,675.59
--
--   payments `low value goods`  (-196.62)
--       == ITEMWITHHELDTAX/LOWVALUEGOODSTAX-PRINCIPAL -193.90
--        + ITEMWITHHELDTAX/LOWVALUEGOODSTAX-SHIPPING    -2.72  -> exactly -196.62
--
-- The payments report gives tax as TWO columns; the settlement report gives it
-- as SIX components. No routing that splits tax across Product Charges /
-- Shipping / Other can ever be reproduced from the payments side, because the
-- payments side does not carry the split. The only bucket at which both
-- reports can agree is a single tax line — and the Summary sheet has exactly
-- one, "Tax" (sales_tax), which is otherwise unreachable in this locale:
-- before this fix the only rules pointing at sales_tax were COMMINGLING_VAT,
-- which does not occur in AU.
--
-- Proof that this is the right bucket, not just a convenient one:
--   payments   13,675.59 + (-196.62)                                = 13,478.97
--   settlement 13,765.71 + 581.68 + 0.36 - 672.16 - 193.90 - 2.72   = 13,478.97
--
-- Effect: Sales>Tax becomes 13,478.97 on both sides; Product Charges and
--         Shipping fall back to pure principal and pure shipping.
-- ===========================================================================

UPDATE settlement_config
   SET summary_field_positive = 'sales_tax',
       summary_field_negative = 'sales_tax'
 WHERE transaction_type = 'ORDER'
   AND (   (amount_type = 'ITEMPRICE'       AND amount_description IN ('TAX','SHIPPINGTAX','GIFTWRAPTAX'))
        OR (amount_type = 'PROMOTION'       AND amount_description = 'TAXDISCOUNT')
        OR (amount_type = 'ITEMWITHHELDTAX' AND amount_description IN ('LOWVALUEGOODSTAX-PRINCIPAL','LOWVALUEGOODSTAX-SHIPPING')));


-- ===========================================================================
-- DEFECT 4 — payments: gift wrap credits routed to a bucket that does not exist
-- ---------------------------------------------------------------------------
-- WAS:
--   ORDER, any, gift_wrap_credits, txn_ref+sku+date, sales_gift_wrap_credits, sales_gift_wrap_credits
--
-- `sales_gift_wrap_credits` is not a line on the Summary sheet and is not used
-- by any other rule in either config. 11.61 of gift wrap revenue was routed
-- into a bucket the report cannot display, so it silently vanished from the
-- Payments column while the settlement side booked it to Sales>Other
-- (ORDER/ITEMPRICE/GIFTWRAP -> sales_other).
--
-- NOW: sales_other, matching the settlement side.
--
-- Effect: Sales>Other +11.61 on the Payments column.
-- ===========================================================================

UPDATE payment_config
   SET summary_field_positive = 'sales_other',
       summary_field_negative = 'sales_other'
 WHERE transaction_type = 'ORDER' AND description = 'ANY'
   AND amount_field = 'GIFT_WRAP_CREDITS';


-- ===========================================================================
-- DEFECT 5 — payments: promotional rebates booked to Expenses>Other
-- ---------------------------------------------------------------------------
-- WAS:
--   ORDER, any, promotional_rebates, txn_ref+sku+date, expenses_other, expenses_other
--
-- The settlement side books the same money to Expenses>Promo rebates
-- (ORDER/PROMOTION/PRINCIPAL and ORDER/PROMOTION/SHIPPING ->
-- expenses_promotional_rebates), and the amounts match exactly:
--   payments `promotional rebates`  -11,273.53
--       == PROMOTION/PRINCIPAL  -4,098.30
--        + PROMOTION/SHIPPING   -7,175.23
--
-- (PROMOTION/TAXDISCOUNT is NOT part of this column — it sits inside
-- `sales tax collected`, which is why defect 3 moves it to Tax and not here.)
--
-- NOW: expenses_promotional_rebates.
--
-- Effect: Expenses>Promo rebates -11,273.53 and Expenses>Other +11,273.53 on
--         the Payments column. Group total unchanged — a reclassification.
-- ===========================================================================

UPDATE payment_config
   SET summary_field_positive = 'expenses_promotional_rebates',
       summary_field_negative = 'expenses_promotional_rebates'
 WHERE transaction_type = 'ORDER' AND description = 'ANY'
   AND amount_field = 'PROMOTIONAL_REBATES';


-- ===========================================================================
-- DEFECT 6 — payments: refund tax is not summarised at all
-- ---------------------------------------------------------------------------
-- WAS:
--   REFUND, any, sales_tax_collected, txn_ref+sku+date, (empty), (empty)
--
-- An empty target means "does not contribute". But the settlement side DOES
-- contribute the same money to Refunds>Refund expenses:
--   payments REFUND `sales tax collected`  -42.59
--       == REFUND/ITEMPRICE/TAX          -41.09
--        + REFUND/ITEMPRICE/SHIPPINGTAX   -3.19
--        + REFUND/PROMOTION/TAXDISCOUNT   +1.69
--
-- So the Payments column was short by exactly 42.59 on Refund expenses.
-- Note the asymmetry with the ORDER side, which does summarise its tax: that
-- inconsistency is what flagged this rule.
--
-- NOW: refunded_expenses on both signs, matching the settlement side.
--
-- Effect: Refunds>Refund expenses -42.59 on the Payments column
--         (258.87 -> 216.28, equal to the settlement side).
-- ===========================================================================

UPDATE payment_config
   SET summary_field_positive = 'refunded_expenses',
       summary_field_negative = 'refunded_expenses'
 WHERE transaction_type = 'REFUND' AND description = 'ANY'
   AND amount_field = 'SALES_TAX_COLLECTED';


-- ===========================================================================
-- DEFECT 7 — settlement: REFUND/ITEMPRICE/TAX routes by sign into two
--            different meanings, one of which is not a Summary line
-- ---------------------------------------------------------------------------
-- WAS:
--   REFUND, ITEMPRICE, TAX, txn_ref+sku+date,
--       to_summary_field_when_positive_amount = total_refund_expense_or_sales_amt
--       to_summary_field_when_negative_amount = refunded_expenses
--
-- Sign-based routing is meant to separate a credit from a charge, not to send
-- the same concept to two unrelated buckets. `total_refund_expense_or_sales_amt`
-- is not a Summary line, so a positive refund tax row would disappear from the
-- report while a negative one lands in Refund expenses.
--
-- In THIS settlement every REFUND/ITEMPRICE/TAX row happens to be negative, so
-- the defect contributes 0.00 today. It is still a defect: one positive row in
-- a future period silently loses money.
--
-- NOW: refunded_expenses on both signs, consistent with SHIPPINGTAX and
--      TAXDISCOUNT on the same transaction type.
--
-- Effect on this settlement: none (0.00). Latent defect closed.
-- ===========================================================================

UPDATE settlement_config
   SET summary_field_positive = 'refunded_expenses'
 WHERE transaction_type = 'REFUND' AND amount_type = 'ITEMPRICE'
   AND amount_description = 'TAX';


-- ===========================================================================
-- DEFECT 8 — CLASSIFICATION DEFECT THAT DOES NOT SHOW AS A VARIANCE
--            FBA per-unit fulfilment fees are reported as Amazon fees
-- ---------------------------------------------------------------------------
-- WAS:
--   payments:   ORDER, any,      fba_fees,                 -> expenses_amazon_fees
--   settlement: ORDER, ITEMFEES, FBAPERUNITFULFILLMENTFEE, -> expenses_amazon_fees
--               ORDER, ITEMFEES, GIFTWRAPCHARGEBACK,       -> expenses_amazon_fees
--               ORDER, ITEMFEES, SHIPPINGCHARGEBACK,       -> expenses_amazon_fees
--
-- Both sides agree, so this produces NO difference between the two Summary
-- columns. It is still wrong, and it is the largest single misclassification
-- in the report: 96,803.17 of FBA fulfilment fees sits on Expenses>Amazon fees
-- while the dedicated Expenses>FBA fees line shows only -0.41.
--
-- Why it is a defect:
--   * Every other FBA fee in BOTH configs routes to expenses_fba_fees — FBA
--     storage, long-term storage, removal, disposal, return, AWD storage /
--     processing / transportation, inbound placement, customer returns fee.
--     The per-unit fulfilment fee, the largest FBA fee of all, is the only
--     one that does not.
--   * Expenses>Amazon fees should be the selling/commission line. After this
--     fix it is -35,790.24 = commission (-35,773.27) + subscription (-16.97),
--     which is what that line is supposed to mean.
--
-- Why it is safe: the payments `fulfilment by amazon fees` column maps exactly
-- onto those three settlement components, verified over the matched groups:
--   payments fba_fees        -96,803.17
--       == FBAPERUNITFULFILLMENTFEE -95,592.26
--        + GIFTWRAPCHARGEBACK            -11.97
--        + SHIPPINGCHARGEBACK         -1,198.94
-- so moving all three together keeps the two columns in exact agreement.
--
-- Caveat, stated plainly: gift wrap and shipping chargebacks are not FBA fees.
-- They travel with the FBA fee because the payments report merges all three
-- into one column, so they cannot be separated from the payments side. They
-- are 1,210.91 of the 96,803.17 (1.3%).
--
-- Effect: Expenses>FBA fees -96,803.17 and Expenses>Amazon fees +96,803.17,
--         identically on both columns. Tie-out to Amazon is unchanged.
--
-- To revert this one block without touching the others, re-run with the three
-- settlement rows and the one payments row set back to expenses_amazon_fees.
-- ===========================================================================

UPDATE payment_config
   SET summary_field_positive = 'expenses_fba_fees',
       summary_field_negative = 'expenses_fba_fees'
 WHERE transaction_type = 'ORDER' AND description = 'ANY'
   AND amount_field = 'FBA_FEES';

UPDATE settlement_config
   SET summary_field_positive = 'expenses_fba_fees',
       summary_field_negative = 'expenses_fba_fees'
 WHERE transaction_type = 'ORDER' AND amount_type = 'ITEMFEES'
   AND amount_description IN ('FBAPERUNITFULFILLMENTFEE','GIFTWRAPCHARGEBACK','SHIPPINGCHARGEBACK');


-- ===========================================================================
-- DEFECT 9 — payments: no rule for the TRANSFER `other` column
-- ---------------------------------------------------------------------------
-- WAS: the config has
--   TRANSFER, TO_YOUR_ACCOUNT_ENDING, total, TRANSFER+description+settlement_id+date, (empty), (empty)
--   TRANSFER, TO_ACCOUNT_ENDING,      total, TRANSFER+description+settlement_id+date, (empty), (empty)
-- and nothing for the `other` column, although Amazon writes the disbursement
-- amount into BOTH `other` and `total` (-231,676.27 across the two transfer
-- rows in this file, -133,756.51 of it in this settlement).
--
-- The outcome happened to be right — an unmapped column contributes nothing,
-- which is what a bank disbursement should contribute to a P&L summary — but
-- it was right by accident, and the ingest reported it as UNMAPPED. A rule
-- that is deliberately empty is very different from a missing rule.
--
-- NOW: explicit rules with empty summary fields, mirroring the `total` rows,
--      so the intent is recorded rather than inferred.
--
-- Effect: none on any number. Removes the UNMAPPED warning.
-- ===========================================================================

-- Scoped to the disbursement descriptions only. TRANSFER / MICRO_DEPOSIT
-- already has its own `other` rule (-> bank_account_transfer_round_off) and
-- must not be given a second, empty one.
INSERT INTO payment_config (id, transaction_type, description, amount_field,
                            record_ref_template, summary_field_positive,
                            summary_field_negative, source_line)
SELECT 900 + ROW_NUMBER() OVER (ORDER BY id),
       transaction_type, description, 'OTHER',
       record_ref_template, '', '', source_line
  FROM payment_config
 WHERE transaction_type = 'TRANSFER' AND amount_field = 'TOTAL'
   AND description LIKE '%ACCOUNT_ENDING';


-- ===========================================================================
-- DEFECT 10 — payments: AWD storage fee record_ref cannot join its settlement
--             counterpart (singular/plural literal)
-- ---------------------------------------------------------------------------
-- WAS:
--   payments:   SERVICE_FEE / AWD_STORAGE_FEES
--                 -> txn_ref+AWD_STORAGE_FEES+settlement_id+date
--   settlement: OTHER-TRANSACTION / AMAZON_WAREHOUSING_&_DISTRIBUTION_(AWD) / AWD_STORAGE_FEE
--                 -> txn_ref+AWD_STORAGE_FEE+settlement_id+date
--
-- The literal segment differs by one character ("FEES" vs "FEE"), so the two
-- sides build different keys for the same fee and could never reconcile. The
-- sibling AWD rules do not have this problem — AWD processing and AWD
-- transportation both use SERVICE_FEE_AWD_*_FEES on both sides — which is what
-- makes this one identifiable as a typo rather than a deliberate split.
--
-- No AWD storage fee occurs in this settlement, so the effect here is 0.00.
-- Fixed anyway: it is unambiguous and would silently split a record the first
-- time an AWD storage fee appears.
--
-- NOW: the payments literal is aligned to the settlement config's spelling.
-- ===========================================================================

UPDATE payment_config
   SET record_ref_template = REPLACE(record_ref_template, 'AWD_STORAGE_FEES', 'AWD_STORAGE_FEE')
 WHERE transaction_type = 'SERVICE_FEE' AND description = 'AWD_STORAGE_FEES';


-- ===========================================================================
-- DEFECT 11 — settlement: malformed record_ref template for PROMOTION_ADJUSTMENT
-- ---------------------------------------------------------------------------
-- WAS:
--   OTHER-TRANSACTION, OTHER-TRANSACTION, PROMOTION_ADJUSTMENT
--     -> ADJUSTMENT_OTHER+settlement_id+date+record_type+settlement_id
--
-- settlement_id appears twice and `record_type` is not a column either report
-- exposes, so the template renders a key no payments row can ever produce. The
-- payments side books the same concept (ADJUSTMENT / OTHER, ADJUSTMENT /
-- COMMISSION_ADJUSTMENT) to ADJUSTMENT_OTHER+settlement_id+date, as does the
-- settlement side's own MISCADJUSTMENT rule.
--
-- No PROMOTION_ADJUSTMENT rows in this settlement, so the effect here is 0.00.
--
-- NOW: aligned to the template every neighbouring rule already uses.
-- ===========================================================================

UPDATE settlement_config
   SET record_ref_template = 'ADJUSTMENT_OTHER+settlement_id+date'
 WHERE transaction_type = 'OTHER-TRANSACTION' AND amount_type = 'OTHER-TRANSACTION'
   AND amount_description = 'PROMOTION_ADJUSTMENT';


COMMIT;


-- ===========================================================================
-- ===========================================================================
--  DEFECTS IDENTIFIED BUT DELIBERATELY NOT FIXED
--  (no statements below this line — these are findings, with what each would
--   need before a fix could be justified)
-- ===========================================================================
-- ===========================================================================


-- ---------------------------------------------------------------------------
-- FINDING A — settlement: failed-disbursement row has NO record_ref template
--             at all, and the "Micro Deposit (Failed)" Summary line is
--             unreachable from either config
-- ---------------------------------------------------------------------------
-- The row:
--   OTHER-TRANSACTION, OTHER-TRANSACTION,
--   TRANSFER_OF_FUNDS_UNSUCCESSFUL:_WE_COULD_NOT_TRANSFER_FUNDS_TO_YOUR_BANK_
--   ACCOUNT_BECAUSE_THE_ACCOUNT_INFORMATION_ON_FILE_IS_INVALID._PLEASE_UPDATE_
--   YOUR_BANK_ACCOUNT_INFORMATION.
--     record_ref = (empty), both summary targets = (empty)
--
-- Two problems:
--   1. An empty record_ref template means the row can never reconcile with
--      anything — it produces no key.
--   2. The Summary sheet has a "Micro Deposit (Failed)" line under Sales, and
--      NO rule in either config routes to it. The payments counterpart
--      (ADJUSTMENT / FAILED_DISBURSEMENT -> FAILED_DISBURSEMENT+description+
--      settlement_id+date) also has empty summary targets.
--
-- Why not fixed: the obvious repair is a shared template plus a route to
-- sales_micro_deposit_failed, but the two sides describe the event with
-- different text ("Failed disbursement" vs "Transfer of funds unsuccessful:
-- ..."), so a `description`-based template will NOT align them, and I have no
-- row of either kind in this settlement to verify against. Guessing here would
-- be exactly the "plugging numbers" the brief rules out.
--
-- What would close it: one settlement period containing a failed
-- disbursement on both reports, to confirm (a) that the shared anchor should be
-- FAILED_DISBURSEMENT+settlement_id+date with `description` dropped, and
-- (b) whether the amount belongs on Sales>Micro Deposit (Failed) or is a pure
-- balance movement that should stay out of the summary.
--
-- Effect on this settlement: 0.00 — neither report contains such a row.


-- ---------------------------------------------------------------------------
-- FINDING B — payments and settlement disagree on the anchor field for two
--             shipment-level fees
-- ---------------------------------------------------------------------------
--   INBOUND_DEFECT_FEE
--     payments:   txn_ref+INBOUND_DEFECT_FEE+settlement_id+date
--     settlement: shipment_id+INBOUND_DEFECT_FEE+settlement_id+date
--   FBA_INBOUND_PLACEMENT_SERVICE_FEE
--     payments:   txn_ref+SERVICE_FEE_FBA_INBOUND_PLACEMENT_SERVICE_FEE+settlement_id+date
--     settlement: shipment_id+SERVICE_FEE_FBA_INBOUND_PLACEMENT_SERVICE_FEE+settlement_id+date
--
-- Different token, therefore potentially different key. But this may be
-- correct as written: for shipment-level fees Amazon puts the shipment id in
-- the payments report's `order ID` column — exactly as it does for the removal
-- order in this file, where payments `order ID` = M0jcPjF2nw equals the
-- settlement `order-id`. If that holds for inbound fees too, txn_ref and
-- shipment_id resolve to the same string and the templates already agree.
--
-- Why not fixed: neither fee occurs in this settlement, so I cannot tell
-- whether these are two spellings of the same value or a genuine mismatch.
-- Changing it blind could break a pair that currently works.
--
-- What would close it: a settlement containing an inbound defect or inbound
-- placement fee, to compare payments `order ID` against settlement
-- `shipment-id` on the same fee.
--
-- Effect on this settlement: 0.00.


-- ---------------------------------------------------------------------------
-- FINDING C — DEALS promotion fee keys do not correspond
-- ---------------------------------------------------------------------------
--   payments:   DEALS / any / total -> txn_ref+settlement_id+date
--   settlement: PROMOTION_FEE|SERVICEFEE / DEALS / PROMOTION_FEE
--                   -> DEALS_PROMOTION_FEE+settlement_id+date
--               ... / PROMOTION_FEE_SPECIAL
--                   -> DEALS_PROMOTION_FEE_SPECIAL+settlement_id+date
--
-- The payments rule keys on the transaction reference while the settlement
-- rules key on a fixed literal, so a deal fee would land as an unreconciled
-- pair on both sides even though both route to expenses_amazon_fees (meaning
-- the Summary columns would still agree — another case where agreement is not
-- proof).
--
-- Why not fixed: the payments config has a single catch-all DEALS rule while
-- the settlement config distinguishes PROMOTION_FEE from
-- PROMOTION_FEE_SPECIAL. Collapsing the settlement side would lose that
-- distinction; expanding the payments side needs to know which Amazon
-- `description` values map to which, and there are no DEALS rows here.
--
-- Effect on this settlement: 0.00.


-- ---------------------------------------------------------------------------
-- FINDING D — settlement-only summary fields with no Summary line
-- ---------------------------------------------------------------------------
--   beginning_balance       <- OTHER-TRANSACTION / PREVIOUS_RESERVE_AMOUNT_BALANCE
--   current_reserve_amount  <- OTHER-TRANSACTION / CURRENT_RESERVE_AMOUNT
--   amazon_carried_forward  <- OTHER-TRANSACTION / PAYABLE_TO_AMAZON
--
-- These are balances, not movements inside the period, and the Summary sheet
-- correctly has no line for them. Not defects — recorded here so the reviewer
-- can see they were considered and excluded on purpose. The pipeline lists
-- them under "Summary fields with no Summary line" on the Summary sheet if
-- they ever carry a value, which in this settlement they do not.
--
-- Effect on this settlement: 0.00 — none of the three occurs.


-- ---------------------------------------------------------------------------
-- FINDING E — total_refund_expense_or_sales_amt /
--             total_adjustment_other_buyer_recharge_amt are used by BOTH
--             configs but are not Summary lines
-- ---------------------------------------------------------------------------
-- Used by, among others:
--   payments   REFUND / gift_wrap_credits, REFUND / other
--   settlement REFUND / ITEMPRICE / GIFTWRAP, GOODWILL, RESTOCKINGFEE
--   both       ADJUSTMENT / OTHER, BUYER_RECHARGE, MISCADJUSTMENT, ...
--
-- Because both configs use them symmetrically, any money routed here is
-- missing from BOTH columns equally — the columns still agree while the
-- Summary quietly understates. In this settlement all of these rules see
-- 0.00, so nothing is lost today; defect 7 removed the one case that could
-- have leaked (REFUND/ITEMPRICE/TAX on a positive amount).
--
-- Why not fixed: routing them needs a decision this data cannot make —
-- whether a restocking fee is a refund expense or a sale, and whether a buyer
-- recharge is Sales>Other or Expenses>Other. Both configs would have to move
-- together, and picking wrong would move real money in a future period.
--
-- What would close it: a settlement containing a restocking fee, a goodwill
-- credit and a buyer recharge, reconciled line by line as was done for tax in
-- defect 3.
--
-- Effect on this settlement: 0.00. The report surfaces any non-zero value in
-- these buckets rather than dropping it.
