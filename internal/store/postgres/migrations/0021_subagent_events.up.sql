-- Subagent events (spec 013): a subagent's events are subagent.started,
-- subagent.ended and subagent.message, and the column that holds the id
-- of the subagent an event belongs to is subagent, empty for the
-- session's own conversation. The rows already written are renamed in
-- place, so a session written before reads and replays as one written
-- after. The partial indexes on the column, events_unsearched and
-- events_activity, follow the rename, since an index names a column by
-- its number. No payload holds an event type or the column's name.
ALTER TABLE events RENAME COLUMN thread TO subagent;

UPDATE events SET type = 'subagent.' || substr(type, length('thread.') + 1)
    WHERE type IN ('thread.started', 'thread.ended', 'thread.message');
