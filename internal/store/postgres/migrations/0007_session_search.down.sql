DROP INDEX events_unsearched;
DROP INDEX events_search;
ALTER TABLE events DROP COLUMN search;
