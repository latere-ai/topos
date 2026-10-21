-- What a day's activity reads (spec 085): the messages and the statuses
-- that end a turn on a session's own thread, found by their time without
-- reading every event, and the sessions by the time they were created.
CREATE INDEX events_activity ON events (time)
    WHERE thread = '' AND type IN ('user.message', 'session.status');

CREATE INDEX sessions_created ON sessions (created_at);
