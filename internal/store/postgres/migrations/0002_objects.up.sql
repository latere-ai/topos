-- Agents with their versions, and the idempotency records of the API's
-- POST routes (spec 014). An agent's latest_version is the commit record
-- of its versions: a version is inserted and the agent's row updated in
-- one transaction under the row's lock. The resolved document and the
-- bundle are JSON text, read and written whole; an idempotency record's
-- body is the stored answer's bytes, of any content type.
CREATE TABLE agents (
    id              text PRIMARY KEY,
    name            text NOT NULL UNIQUE,
    owner           text NOT NULL,
    latest_version  integer NOT NULL,
    archived_at     timestamptz,
    created_at      timestamptz NOT NULL
);

CREATE INDEX agents_owner ON agents (owner, id);

CREATE TABLE agent_versions (
    agent_id    text NOT NULL REFERENCES agents (id),
    version     integer NOT NULL,
    digest      text NOT NULL,
    doc         text NOT NULL,
    bundle      text NOT NULL,
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (agent_id, version)
);

CREATE TABLE idempotency_keys (
    subject       text NOT NULL,
    key           text NOT NULL,
    route         text NOT NULL,
    body_hash     text NOT NULL,
    done          boolean NOT NULL DEFAULT false,
    status        integer NOT NULL DEFAULT 0,
    content_type  text NOT NULL DEFAULT '',
    body          bytea,
    expires_at    timestamptz NOT NULL,
    PRIMARY KEY (subject, key)
);

-- A list narrowed to its owners, the initiators' subjects, walks this
-- index newest first. The runner filter has two values and walks the
-- primary key.
CREATE INDEX sessions_owner ON sessions (owner, id DESC);
