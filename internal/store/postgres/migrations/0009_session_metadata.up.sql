-- A session's metadata entries as rows (spec 057), so a list filtered by
-- one entry reads an index instead of every header. The body keeps the
-- metadata as before; a create and a change write the rows in the
-- transaction that writes the body, so the two never disagree.
CREATE TABLE session_metadata (
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    key        text NOT NULL,
    value      text NOT NULL,
    PRIMARY KEY (session_id, key)
);
CREATE INDEX session_metadata_entry ON session_metadata (key, value, session_id DESC);

INSERT INTO session_metadata (session_id, key, value)
SELECT s.id, m.key, m.value
FROM sessions s, jsonb_each_text(s.body::jsonb -> 'metadata') AS m(key, value)
WHERE jsonb_typeof(s.body::jsonb -> 'metadata') = 'object';
