-- An agent's name is unique within its owner, not across the
-- installation: two owners may each hold an agent of one name, and a
-- name is read within the caller's own agents (spec 015). The
-- installation-wide constraint is the stricter of the two, so every
-- stored row holds under this one. Its index serves a lookup of an
-- owner's name; agents_owner (owner, id) still serves the list.
ALTER TABLE agents DROP CONSTRAINT agents_name_key;
ALTER TABLE agents ADD CONSTRAINT agents_owner_name_key UNIQUE (owner, name);
