-- A session's archived_at files it away from the lists (spec 015). The
-- body carries it as every other field; the column is the filter a
-- list reads, NULL for a session that is not archived.
ALTER TABLE sessions ADD COLUMN archived_at timestamptz;
