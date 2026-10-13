-- The trigger that started a session (spec 072), as a column a list
-- filtered by trigger reads an index by instead of every header. The
-- body keeps trigger_id as before; a create writes the column beside it,
-- and a save of the header fills it where a replica of an earlier
-- release created the session without it. NULL on a session no trigger
-- started.
ALTER TABLE sessions ADD COLUMN trigger_id text;

UPDATE sessions SET trigger_id = body::jsonb ->> 'trigger_id'
    WHERE COALESCE(body::jsonb ->> 'trigger_id', '') <> '';

CREATE INDEX sessions_trigger ON sessions (trigger_id, id DESC) WHERE trigger_id IS NOT NULL;
