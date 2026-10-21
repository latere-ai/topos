-- What a day's activity reads (spec 085): the messages and the statuses
-- that end a turn on a session's own thread, found by their time without
-- reading every event. The index is built concurrently, so the appends of
-- a running installation are not held while it builds; it is the file's
-- one statement, since a concurrent build runs outside a transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_activity ON events (time)
    WHERE thread = '' AND type IN ('user.message', 'session.status');
