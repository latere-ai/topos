-- Sessions, their logs and their blobs (spec 014). JSON is kept as text:
-- the store reads and writes the objects whole and filters on the
-- columns, so no query needs jsonb.
CREATE TABLE sessions (
    id                text PRIMARY KEY,
    agent_id          text NOT NULL,
    agent_version     integer NOT NULL,
    owner             text NOT NULL,
    runner            text NOT NULL,
    status            text NOT NULL,
    stop_reason       text NOT NULL DEFAULT '',
    turn              integer NOT NULL DEFAULT 0,
    last_seq          bigint NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    expires_at        timestamptz NOT NULL,
    ended_at          timestamptz,
    body              text NOT NULL,
    wake              boolean NOT NULL DEFAULT false,
    lease_holder      text,
    lease_generation  bigint NOT NULL DEFAULT 0,
    lease_expires_at  timestamptz,
    writer_kind       text NOT NULL DEFAULT '',
    writer_subject    text NOT NULL DEFAULT ''
);

CREATE INDEX sessions_agent ON sessions (agent_id, id DESC);
CREATE INDEX sessions_status ON sessions (status, id DESC);

CREATE TABLE events (
    session_id  text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    seq         bigint NOT NULL,
    id          text NOT NULL,
    type        text NOT NULL,
    time        timestamptz NOT NULL,
    thread      text NOT NULL DEFAULT '',
    turn        integer NOT NULL DEFAULT 0,
    step        integer NOT NULL DEFAULT 0,
    payload     text NOT NULL,
    redacted    boolean NOT NULL DEFAULT false,
    PRIMARY KEY (session_id, seq),
    UNIQUE (session_id, id)
);

CREATE TABLE blobs (
    session_id  text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    digest      text NOT NULL,
    size        bigint NOT NULL,
    location    text NOT NULL DEFAULT 'db',
    body        bytea,
    PRIMARY KEY (session_id, digest)
);
