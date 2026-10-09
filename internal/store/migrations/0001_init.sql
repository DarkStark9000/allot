-- One row per order. The state machine lives in Go; this table holds its current state.
CREATE TABLE orders (
    id              uuid        PRIMARY KEY,
    user_id         text        NOT NULL,
    idempotency_key text        NOT NULL,
    scheme_code     text        NOT NULL,
    category        text        NOT NULL,
    amount_paise    bigint      NOT NULL CHECK (amount_paise > 0),
    state           text        NOT NULL,
    version         int         NOT NULL DEFAULT 0,
    payment_id      text,
    paid_at         timestamptz,
    nav_date        date,
    nav             bigint,
    units_milli     bigint,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, idempotency_key)
);
CREATE INDEX orders_open ON orders (state, created_at)
    WHERE state NOT IN ('allotted', 'refunded', 'payment_failed', 'expired');

-- Append-only history: one row per transition. seq equals the order's version after it.
CREATE TABLE order_events (
    order_id   uuid        NOT NULL REFERENCES orders (id),
    seq        int         NOT NULL,
    from_state text        NOT NULL,
    to_state   text        NOT NULL,
    event_kind text        NOT NULL,
    source     text        NOT NULL,
    event_id   text        NOT NULL,
    at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (order_id, seq)
);

-- Idempotency keys for POST /v1/orders, per the IETF Idempotency-Key draft.
CREATE TABLE idempotency_keys (
    user_id       text        NOT NULL,
    key           text        NOT NULL,
    fingerprint   bytea       NOT NULL,
    completed     boolean     NOT NULL DEFAULT false,
    response_code int,
    response_body bytea,
    locked_until  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);

-- Every inbound event, once. A second delivery hits the primary key and changes nothing.
CREATE TABLE inbox_events (
    source      text        NOT NULL,
    event_id    text        NOT NULL,
    order_id    uuid,
    kind        text        NOT NULL,
    payload     jsonb       NOT NULL,
    outcome     text        NOT NULL DEFAULT 'received',
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source, event_id)
);

-- Work for the relay, written in the same transaction as the state change that caused it.
CREATE TABLE outbox (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id     uuid        NOT NULL REFERENCES orders (id),
    kind         text        NOT NULL,
    available_at timestamptz NOT NULL DEFAULT now(),
    lease_until  timestamptz,
    attempts     int         NOT NULL DEFAULT 0,
    last_error   text,
    sent_at      timestamptz
);
CREATE INDEX outbox_pending ON outbox (available_at, id) WHERE sent_at IS NULL;

-- Double entry. Each row moves amount_paise from credit_account to debit_account.
CREATE TABLE ledger_entries (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id       uuid        NOT NULL REFERENCES orders (id),
    seq            int         NOT NULL,
    debit_account  text        NOT NULL,
    credit_account text        NOT NULL,
    amount_paise   bigint      NOT NULL CHECK (amount_paise > 0),
    posted_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (debit_account <> credit_account)
);
CREATE INDEX ledger_by_order ON ledger_entries (order_id);

-- Things a person must look at. One open finding per order and kind.
CREATE TABLE recon_findings (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id    uuid        NOT NULL REFERENCES orders (id),
    kind        text        NOT NULL,
    detail      text        NOT NULL,
    found_at    timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz
);
CREATE UNIQUE INDEX findings_open ON recon_findings (order_id, kind) WHERE resolved_at IS NULL;
