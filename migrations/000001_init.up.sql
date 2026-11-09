CREATE TABLE events (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name                  text        NOT NULL,
    scenario              text        NOT NULL DEFAULT '',
    variant               text        NOT NULL DEFAULT '',
    confirmed_capacity    int         NOT NULL CHECK (confirmed_capacity >= 0),
    deadline_at           timestamptz NOT NULL,
    deadline_processed    boolean     NOT NULL DEFAULT false,
    deadline_processed_at timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_events_due ON events (deadline_at) WHERE NOT deadline_processed;

CREATE TABLE allocations (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id       uuid        NOT NULL REFERENCES events (id),
    user_id        text        NOT NULL,
    kind           text        NOT NULL CHECK (kind IN ('confirmed', 'waitlist')),
    status         text        NOT NULL CHECK (status IN ('active', 'cancelled', 'converted', 'refunded')),
    purchased_at   timestamptz NOT NULL,
    cancelled_at   timestamptz,
    converted_from uuid REFERENCES allocations (id),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_allocations_event_kind_status ON allocations (event_id, kind, status);

CREATE INDEX idx_allocations_waitlist_fifo
    ON allocations (event_id, purchased_at, id)
    WHERE kind = 'waitlist' AND status = 'active';

CREATE UNIQUE INDEX uniq_active_confirmed_per_user
    ON allocations (event_id, user_id)
    WHERE kind = 'confirmed' AND status = 'active';

CREATE TABLE conversion_jobs (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id           uuid        NOT NULL REFERENCES events (id),
    cancelled_alloc_id uuid        NOT NULL REFERENCES allocations (id),
    status             text        NOT NULL CHECK (status IN ('pending', 'processing', 'done', 'failed')),
    attempts           int         NOT NULL DEFAULT 0,
    claimed_by         text,
    claimed_at         timestamptz,
    completed_at       timestamptz,
    idempotency_key    text        NOT NULL UNIQUE,
    converted_alloc_id uuid REFERENCES allocations (id),
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_conversion_jobs_open ON conversion_jobs (event_id, status) WHERE status IN ('pending', 'processing');

CREATE TABLE refund_ledger (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    allocation_id   uuid        NOT NULL REFERENCES allocations (id),
    status          text        NOT NULL CHECK (status IN ('pending', 'completed', 'failed')),
    idempotency_key text        NOT NULL UNIQUE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    completed_at    timestamptz
);

CREATE INDEX idx_refund_ledger_allocation ON refund_ledger (allocation_id);

ALTER TABLE events          ENABLE ROW LEVEL SECURITY;
ALTER TABLE allocations     ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversion_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE refund_ledger   ENABLE ROW LEVEL SECURITY;
