-- A run of a trigger whose reach splits by scope (spec 095): one firing
-- is a run per scope it started or the authorizer refused, so a run is
-- keyed by its firing and its scope, empty for every other run.
ALTER TABLE trigger_runs ADD COLUMN scope text NOT NULL DEFAULT '';
ALTER TABLE trigger_runs DROP CONSTRAINT trigger_runs_pkey;
ALTER TABLE trigger_runs ADD PRIMARY KEY (firing_id, scope);
