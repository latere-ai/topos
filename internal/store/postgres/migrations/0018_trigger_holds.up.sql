-- An owner's hold over every trigger they own (spec 086): while a row
-- names the owner, the minute loop fires none of their triggers and the
-- fire route refuses each.
CREATE TABLE trigger_holds (
    owner  text PRIMARY KEY,
    since  timestamptz NOT NULL
);
