-- PortOne Prism — Amazon Payments vs Settlement reconciliation
-- Schema. See README.md ("Schema design") for the rationale behind each table.

DROP TABLE IF EXISTS summary_bucket;
DROP TABLE IF EXISTS ledger_entry;
DROP TABLE IF EXISTS settlement_config;
DROP TABLE IF EXISTS payment_config;
DROP TABLE IF EXISTS ingest_run;

-- ---------------------------------------------------------------------------
-- Mapping configuration. Held as data so that step-5 fixes are UPDATE/INSERT/
-- DELETE statements rather than code changes.
-- ---------------------------------------------------------------------------

CREATE TABLE payment_config (
    id                      integer PRIMARY KEY,
    transaction_type        text NOT NULL DEFAULT '', -- '' = catch-all / fallback
    description             text NOT NULL DEFAULT '', -- 'ANY' = wildcard
    amount_field            text NOT NULL,            -- which payments column this rule consumes
    record_ref_template     text NOT NULL,
    summary_field_positive  text NOT NULL DEFAULT '', -- '' = does not contribute
    summary_field_negative  text NOT NULL DEFAULT '',
    source_line             integer NOT NULL,
    UNIQUE (transaction_type, description, amount_field, summary_field_positive, summary_field_negative)
);

CREATE TABLE settlement_config (
    id                      integer PRIMARY KEY,
    transaction_type        text NOT NULL DEFAULT '',
    amount_type             text NOT NULL DEFAULT '',
    amount_description      text NOT NULL DEFAULT '', -- 'ANY' = wildcard
    record_ref_template     text NOT NULL,
    summary_field_positive  text NOT NULL DEFAULT '',
    summary_field_negative  text NOT NULL DEFAULT '',
    source_line             integer NOT NULL,
    UNIQUE (transaction_type, amount_type, amount_description, summary_field_positive, summary_field_negative)
);

CREATE INDEX payment_config_lookup    ON payment_config (transaction_type, amount_field);
CREATE INDEX settlement_config_lookup ON settlement_config (transaction_type, amount_type);

-- ---------------------------------------------------------------------------
-- Ingest runs. Every ledger row belongs to exactly one run, which is what makes
-- re-ingestion idempotent: a re-run of the same (file, checksum) pair replaces
-- the previous run's rows wholesale instead of appending duplicates.
-- ---------------------------------------------------------------------------

CREATE TABLE ingest_run (
    id              bigserial PRIMARY KEY,
    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,
    payments_file   text NOT NULL,
    payments_sha256 text NOT NULL,
    settlements_file   text NOT NULL,
    settlements_sha256 text NOT NULL,
    config_revision text NOT NULL,          -- 'before-fix' / 'after-fix'
    rows_payment    integer NOT NULL DEFAULT 0,
    rows_settlement integer NOT NULL DEFAULT 0,
    notes           text NOT NULL DEFAULT ''
);

-- ---------------------------------------------------------------------------
-- The single ledger table. BOTH source files land here (assignment requirement).
--
-- Grain: one row per *amount component*.
--   * A settlement row is already one amount component -> 1 ledger row.
--   * A payments row spreads amounts across typed columns -> it fans out into
--     one ledger row per populated column, which is exactly the shape the
--     payment config addresses via `amount_field`. This is what makes a single
--     table honest rather than a union of two different things.
-- ---------------------------------------------------------------------------

CREATE TABLE ledger_entry (
    id                  bigserial PRIMARY KEY,
    run_id              bigint NOT NULL REFERENCES ingest_run(id) ON DELETE CASCADE,

    -- provenance: every number in the report traces back to these three columns
    source              text NOT NULL CHECK (source IN ('PAYMENT','SETTLEMENT')),
    source_file         text NOT NULL,
    source_line         integer NOT NULL,   -- 1-based line number in the raw file
    raw_payload         jsonb NOT NULL,     -- the complete source row, verbatim

    -- identity / grouping columns (normalised)
    settlement_id       text NOT NULL DEFAULT '',
    txn_ref             text NOT NULL DEFAULT '',   -- order-id / removal-order-id
    merchant_order_id   text NOT NULL DEFAULT '',
    adjustment_id       text NOT NULL DEFAULT '',
    shipment_id         text NOT NULL DEFAULT '',
    sku                 text NOT NULL DEFAULT '',
    quantity            integer,

    -- classification (normalised: upper-cased, whitespace -> '_')
    transaction_type    text NOT NULL DEFAULT '',
    description         text NOT NULL DEFAULT '',   -- payments 'description'
    amount_type         text NOT NULL DEFAULT '',   -- settlement 'amount-type'
    amount_description  text NOT NULL DEFAULT '',   -- settlement 'amount-description'
    amount_field        text NOT NULL DEFAULT '',   -- payments column name

    -- raw (un-normalised) passthroughs kept for the audit sheet
    transaction_type_raw text NOT NULL DEFAULT '',
    description_raw      text NOT NULL DEFAULT '',
    marketplace          text NOT NULL DEFAULT '',
    fulfilment           text NOT NULL DEFAULT '',
    order_city           text NOT NULL DEFAULT '',
    order_state          text NOT NULL DEFAULT '',
    order_postal         text NOT NULL DEFAULT '',
    transaction_status   text NOT NULL DEFAULT '',  -- payments: Released / Deferred

    -- money & time
    amount              numeric(18,4) NOT NULL,
    posted_at           timestamptz,                -- payments date/time, settlement posted-date-time
    released_at         timestamptz,                -- payments Transaction Release Date
    event_date          date,                       -- the normalised `date` token (UTC)

    -- config resolution
    record_ref          text NOT NULL DEFAULT '',
    summary_field       text NOT NULL DEFAULT '',   -- '' = ingested but not summarised
    config_id           integer,
    match_kind          text NOT NULL DEFAULT 'UNMAPPED',
        -- EXACT | WILDCARD | PREFIX | FALLBACK | UNMAPPED
    -- A single amount component can be matched by more than one config rule.
    -- That is a config defect (it double-counts the amount), but the engine
    -- reproduces it faithfully rather than silently picking one: each extra
    -- rule produces another row with route_seq > 0. So:
    --   route_seq = 0  -> the distinct amount components actually in the file
    --   route_seq > 0  -> duplicate routing introduced by the config
    route_seq           smallint NOT NULL DEFAULT 0,
    in_scope            boolean NOT NULL DEFAULT false
);

CREATE INDEX ledger_record_ref  ON ledger_entry (run_id, record_ref);
CREATE INDEX ledger_source      ON ledger_entry (run_id, source);
CREATE INDEX ledger_summary     ON ledger_entry (run_id, summary_field);
CREATE INDEX ledger_settlement  ON ledger_entry (run_id, settlement_id);
CREATE INDEX ledger_unmapped    ON ledger_entry (run_id, match_kind) WHERE match_kind = 'UNMAPPED';

-- ---------------------------------------------------------------------------
-- Summary accumulated *during* ingestion (one pass, no post-hoc aggregation).
-- ---------------------------------------------------------------------------

CREATE TABLE summary_bucket (
    run_id          bigint NOT NULL REFERENCES ingest_run(id) ON DELETE CASCADE,
    source          text NOT NULL CHECK (source IN ('PAYMENT','SETTLEMENT')),
    summary_field   text NOT NULL,
    amount          numeric(18,4) NOT NULL,
    entry_count     integer NOT NULL,
    PRIMARY KEY (run_id, source, summary_field)
);
