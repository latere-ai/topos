-- Clears what a build of the events' activity index left behind (spec 085):
-- a concurrent build that is stopped leaves an invalid index, which the
-- next create would skip. No valid index of this name exists before 0015,
-- so dropping it here only ever drops such a leftover. One statement, since
-- a concurrent drop runs outside a transaction.
DROP INDEX CONCURRENTLY IF EXISTS events_activity;
