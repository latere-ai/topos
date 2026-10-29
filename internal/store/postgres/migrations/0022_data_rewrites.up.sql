-- The rewrites of stored data a server makes once, in Go, after the
-- migrations: a row per rewrite done, written in the transaction that
-- does it, so the server that holds the lock does it and every later
-- start passes it by. subagent_limits renames spec.threads to
-- spec.subagentLimits in every agent version and every session's agent
-- (spec 013), which a statement cannot do: each version's digest and each
-- session's agent blobs are hashes over the spec as Go renders it.
CREATE TABLE data_rewrites (
    name     text PRIMARY KEY,
    done_at  timestamptz NOT NULL
);
