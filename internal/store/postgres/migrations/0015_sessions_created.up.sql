-- The sessions a day's activity counts (spec 085), found by the time they
-- were created, built concurrently as the events' index is.
CREATE INDEX CONCURRENTLY IF NOT EXISTS sessions_created ON sessions (created_at);
