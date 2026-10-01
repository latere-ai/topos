-- Dropping the column loses whether an organization owns an agent; every
-- agent then reads as its owner subject's alone.
ALTER TABLE agents DROP COLUMN owner_type;
