CREATE TABLE sent_documents (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    sha256 BLOB NOT NULL,
    PRIMARY KEY(session_id, path, sha256)
) WITHOUT ROWID;
