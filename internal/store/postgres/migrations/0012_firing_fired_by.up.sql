-- The key whose token fired a firing (spec 077), as the fired_by JSON
-- its session's create carries: kept with the firing so a session
-- started after that token is gone, a held firing's, names the same key.
-- '' for a firing no key's token fired, every firing written before this.
ALTER TABLE trigger_firings ADD COLUMN fired_by text NOT NULL DEFAULT '';
