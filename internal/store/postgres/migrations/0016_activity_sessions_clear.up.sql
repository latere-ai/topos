-- Clears what a build of the sessions' creation index left behind, as
-- 0014 does for the events' (spec 085).
DROP INDEX CONCURRENTLY IF EXISTS sessions_created;
