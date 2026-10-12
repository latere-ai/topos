-- The digest index of a session's files store folder (spec 068): for each
-- file an event records there, its ref and its digest, so the blob route
-- reads a file of the folder by its digest. The append that records a
-- file writes its row in the same transaction; a redaction that leaves no
-- event naming a digest removes its rows, and a session's delete all of
-- them.
CREATE TABLE session_files (
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    ref        text NOT NULL,
    digest     text NOT NULL,
    PRIMARY KEY (session_id, ref)
);
CREATE INDEX session_files_digest ON session_files (session_id, digest);
