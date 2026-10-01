-- An agent belongs to a person or to an organization, and owner_type
-- says which of the two its owner subject is (spec 035). Every row
-- before it is a person's, which the default records.
ALTER TABLE agents ADD COLUMN owner_type text NOT NULL DEFAULT 'user'
    CHECK (owner_type IN ('user', 'organization'));
