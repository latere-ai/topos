-- A session's place in its fork tree (spec 056): the session it was
-- forked from and how many of that session's events it copied, and the
-- session at the top of its tree. The body carries parent and root as
-- every other field; the columns are the filters a list reads, NULL on a
-- session no fork made. A store writes them at a fork's insert and an
-- update never clears them, so a replica of an earlier release, whose
-- header drops root, leaves the column for a read to fill the body from.
ALTER TABLE sessions ADD COLUMN parent_id text, ADD COLUMN parent_seq bigint, ADD COLUMN root_id text;

UPDATE sessions SET parent_id = body::jsonb -> 'parent' ->> 'session_id', parent_seq = (body::jsonb -> 'parent' ->> 'seq')::bigint
    WHERE body::jsonb -> 'parent' ->> 'session_id' IS NOT NULL;

-- Each fork's root is the last session the walk up parent_id reaches: a
-- session no fork made, or one no longer in the table, whose id a fork
-- below it keeps as its tree's key.
WITH RECURSIVE up (id, ancestor, depth) AS (
    SELECT id, parent_id, 1 FROM sessions WHERE parent_id IS NOT NULL
    UNION ALL
    SELECT up.id, s.parent_id, up.depth + 1 FROM up JOIN sessions s ON s.id = up.ancestor WHERE s.parent_id IS NOT NULL
), top AS (
    SELECT DISTINCT ON (id) id, ancestor FROM up ORDER BY id, depth DESC
)
UPDATE sessions SET root_id = top.ancestor FROM top WHERE sessions.id = top.id;

UPDATE sessions SET body = jsonb_set(body::jsonb, '{root}', to_jsonb(root_id))::text WHERE root_id IS NOT NULL;

CREATE INDEX sessions_root ON sessions (root_id, id DESC) WHERE root_id IS NOT NULL;
CREATE INDEX sessions_parent ON sessions (parent_id, id DESC) WHERE parent_id IS NOT NULL;
