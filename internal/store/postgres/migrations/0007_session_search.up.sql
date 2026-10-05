-- What a search reads of an event (spec 050): the words of a message of
-- the session's own thread, or of a person's answer, as the server reads
-- them, written at the append. NULL for every other event and for a
-- redacted one. The GIN index finds the events that hold a query.
ALTER TABLE events ADD COLUMN search tsvector;
CREATE INDEX events_search ON events USING gin (search);

-- The searched events no server has indexed yet: those written before the
-- column, and those a replica of an earlier release writes while this one
-- rolls out. The server indexes them in the background; the types are
-- session.SearchedTypes, which the backfill's query names alike.
CREATE INDEX events_unsearched ON events (session_id, seq)
    WHERE search IS NULL AND NOT redacted AND thread = ''
    AND type IN ('user.message', 'agent.message', 'user.answer');
