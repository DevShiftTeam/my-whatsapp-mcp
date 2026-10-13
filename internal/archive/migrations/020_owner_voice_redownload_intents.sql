-- Explicit owner recovery is independent of historical initial-download rows.
CREATE TABLE owner_voice_redownload_intents(
 id TEXT PRIMARY KEY CHECK(length(id) BETWEEN 1 AND 128),
 account TEXT NOT NULL CHECK(length(CAST(account AS BLOB)) BETWEEN 1 AND 512),
 message_id TEXT NOT NULL CHECK(length(message_id) BETWEEN 1 AND 128),
 media_id TEXT NOT NULL CHECK(length(media_id) BETWEEN 1 AND 128),
 local_revision INTEGER NOT NULL CHECK(local_revision>0),
 policy_generation INTEGER NOT NULL CHECK(policy_generation>0),
 snapshot BLOB NOT NULL CHECK(typeof(snapshot)='blob' AND length(snapshot)<=8192),
 created_at INTEGER NOT NULL,
 request_id TEXT NOT NULL DEFAULT '' CHECK(length(request_id)<=128),
 request_expires_at INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('prepared','awaiting_phone','ready','leased','complete','failed','cancelled','expired')),
 attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 8),
 next_due INTEGER NOT NULL DEFAULT 0,
 lease_id TEXT NOT NULL DEFAULT '' CHECK(length(lease_id)<=128),
 lease_until INTEGER NOT NULL DEFAULT 0,
 protocol_reference BLOB NOT NULL DEFAULT X'' CHECK(typeof(protocol_reference)='blob' AND length(protocol_reference)<=8192),
 protocol_sha256 TEXT NOT NULL DEFAULT '' CHECK(length(protocol_sha256)<=64),
 reservation_id TEXT NOT NULL DEFAULT '' CHECK(length(reservation_id)<=128),
 artifact_id TEXT NOT NULL DEFAULT '' CHECK(length(artifact_id)<=128),
 completed_sha256 TEXT NOT NULL DEFAULT '' CHECK(length(completed_sha256)<=64),
 last_code TEXT NOT NULL DEFAULT '' CHECK(length(last_code)<=64)
);
CREATE UNIQUE INDEX owner_voice_request ON owner_voice_redownload_intents(request_id) WHERE request_id<>'';
CREATE UNIQUE INDEX owner_voice_active_tuple ON owner_voice_redownload_intents(account,media_id,local_revision,policy_generation) WHERE state IN ('prepared','awaiting_phone','ready','leased');
CREATE UNIQUE INDEX owner_voice_bound_artifact ON owner_voice_redownload_intents(artifact_id) WHERE artifact_id<>'';
CREATE INDEX owner_voice_due ON owner_voice_redownload_intents(account,state,next_due);
CREATE INDEX owner_voice_source ON owner_voice_redownload_intents(message_id);
CREATE INDEX owner_voice_media ON owner_voice_redownload_intents(media_id);
