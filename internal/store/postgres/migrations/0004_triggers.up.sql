-- Triggers, their firings and the open session each key names (spec 014,
-- spec 022). A trigger's row holds its latest resolved document as JSON
-- text, read and written whole, the claims of its apply that its
-- firings forward as JSON text, its schedule's next fire time, its
-- firing record and the lease that serializes its firings across
-- replicas; counts is the JSON object of one count per outcome. A firing
-- is keyed by its trigger and its dedupe string, so one firing acts once
-- across replicas and redeliveries.
CREATE TABLE triggers (
    id                text PRIMARY KEY,
    name              text NOT NULL,
    owner             text NOT NULL,
    claims            text NOT NULL DEFAULT '{}',
    agent_id          text NOT NULL,
    version           integer NOT NULL,
    digest            text NOT NULL,
    doc               text NOT NULL,
    suspended         boolean NOT NULL DEFAULT false,
    next_fire_at      timestamptz,
    last_fired_at     timestamptz,
    last_session_id   text NOT NULL DEFAULT '',
    counts            text NOT NULL DEFAULT '{}',
    lease_holder      text NOT NULL DEFAULT '',
    lease_expires_at  timestamptz,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    CONSTRAINT triggers_owner_name_key UNIQUE (owner, name)
);

-- The minute loop reads the due schedules soonest first.
CREATE INDEX triggers_due ON triggers (next_fire_at) WHERE next_fire_at IS NOT NULL AND NOT suspended;

CREATE TABLE trigger_firings (
    trigger_id   text NOT NULL REFERENCES triggers (id) ON DELETE CASCADE,
    dedupe       text NOT NULL,
    id           text NOT NULL UNIQUE,
    origin       text NOT NULL,
    envelope     text NOT NULL DEFAULT '',
    key          text NOT NULL DEFAULT '',
    outcome      text NOT NULL DEFAULT '',
    reason       text NOT NULL DEFAULT '',
    session_id   text NOT NULL DEFAULT '',
    session_open boolean NOT NULL DEFAULT false,
    replica      text NOT NULL DEFAULT '',
    fired_at     timestamptz NOT NULL,
    received_at  timestamptz NOT NULL,
    PRIMARY KEY (trigger_id, dedupe)
);

-- A trigger's firings list newest first; held and open ones are read by
-- the sweep and by the count of a trigger's active sessions.
CREATE INDEX trigger_firings_list ON trigger_firings (trigger_id, id DESC);
CREATE INDEX trigger_firings_held ON trigger_firings (id) WHERE outcome = 'held';
CREATE INDEX trigger_firings_open ON trigger_firings (trigger_id, session_id) WHERE session_open;

CREATE TABLE trigger_sessions (
    trigger_id  text NOT NULL REFERENCES triggers (id) ON DELETE CASCADE,
    key         text NOT NULL,
    session_id  text NOT NULL,
    PRIMARY KEY (trigger_id, key)
);
