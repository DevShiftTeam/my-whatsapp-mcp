CREATE TABLE send_permission_modes(
 credential_id TEXT PRIMARY KEY REFERENCES credentials(id) ON DELETE CASCADE,
 credential_revision INTEGER NOT NULL CHECK(typeof(credential_revision)='integer' AND credential_revision>0),
 mode TEXT NOT NULL CHECK(mode IN('none','selected','all')),
 revision INTEGER NOT NULL CHECK(typeof(revision)='integer' AND revision>0)
);
