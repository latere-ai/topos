DROP INDEX sessions_parent;
DROP INDEX sessions_root;
ALTER TABLE sessions DROP COLUMN root_id, DROP COLUMN parent_seq, DROP COLUMN parent_id;
