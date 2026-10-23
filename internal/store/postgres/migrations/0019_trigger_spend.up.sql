-- A trigger's week as the server last counted it (spec 087): the JSON
-- text of its status.spend, written at each firing that counts toward
-- it; empty before any.
ALTER TABLE triggers ADD COLUMN spend text NOT NULL DEFAULT '';
