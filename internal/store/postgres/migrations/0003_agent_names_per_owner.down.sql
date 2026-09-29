-- The installation-wide name constraint, which refuses to build while two
-- owners hold an agent of one name.
ALTER TABLE agents DROP CONSTRAINT agents_owner_name_key;
ALTER TABLE agents ADD CONSTRAINT agents_name_key UNIQUE (name);
