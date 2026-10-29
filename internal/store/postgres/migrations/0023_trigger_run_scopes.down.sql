-- Back to a run per firing: the runs of every scope but the first of a
-- firing go, since a firing then holds one row.
DELETE FROM trigger_runs AS r USING trigger_runs AS o
 WHERE r.firing_id = o.firing_id AND o.scope COLLATE "C" < r.scope COLLATE "C";
ALTER TABLE trigger_runs DROP CONSTRAINT trigger_runs_pkey;
ALTER TABLE trigger_runs ADD PRIMARY KEY (firing_id);
ALTER TABLE trigger_runs DROP COLUMN scope;
