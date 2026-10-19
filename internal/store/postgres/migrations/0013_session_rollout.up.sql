-- The rollout a session is one run of (spec 077), as a column a list
-- reads an index by: the default list leaves rollout sessions out, and a
-- list by rollout keeps one rollout's. The body keeps rollout as well; a
-- create writes the column beside it. NULL on a session of no rollout.
ALTER TABLE sessions ADD COLUMN rollout text;

UPDATE sessions SET rollout = body::jsonb ->> 'rollout'
    WHERE COALESCE(body::jsonb ->> 'rollout', '') <> '';

CREATE INDEX sessions_rollout ON sessions (rollout, id DESC) WHERE rollout IS NOT NULL;
