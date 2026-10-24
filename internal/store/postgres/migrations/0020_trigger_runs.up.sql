-- A trigger's runs (spec 088): one row per firing a person counts as a
-- run, a turn a trigger's message began or a firing an allowance or the
-- authorizer stopped, with what the turn cost once its log was read. A
-- row is keyed by its firing and does not reference the trigger, since a
-- wake's firing deletes its trigger and the run outlives it; the minute
-- loop deletes the rows older than trigger.RunsKept.
CREATE TABLE trigger_runs (
    firing_id           text PRIMARY KEY,
    owner               text NOT NULL,
    trigger_id          text NOT NULL,
    session_id          text NOT NULL DEFAULT '',
    wake                boolean NOT NULL DEFAULT false,
    session_trigger_id  text NOT NULL DEFAULT '',
    at                  timestamptz NOT NULL,
    outcome             text NOT NULL,
    reason              text NOT NULL DEFAULT '',
    cost_usd_micro      bigint NOT NULL DEFAULT 0,
    ended               boolean NOT NULL DEFAULT false
);

-- The read answers one owner's runs from a time; the minute loop prunes
-- by time.
CREATE INDEX trigger_runs_owner ON trigger_runs (owner, at);
CREATE INDEX trigger_runs_at ON trigger_runs (at);
