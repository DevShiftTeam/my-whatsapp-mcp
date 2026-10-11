-- Explicit offline owner acceptance of a committed, potentially incomplete
-- snapshot. Neither reconnect nor ordinary startup completes this protocol.
ALTER TABLE archive_meta ADD COLUMN recovery_epoch INTEGER NOT NULL DEFAULT 0 CHECK(typeof(recovery_epoch)='integer' AND recovery_epoch>=0);
ALTER TABLE archive_meta ADD COLUMN recovery_code TEXT NOT NULL DEFAULT '' CHECK(recovery_code IN ('','owner_restored_snapshot'));
CREATE TABLE snapshot_recovery_audit (
 recovery_epoch INTEGER PRIMARY KEY CHECK(recovery_epoch>0),
 account TEXT NOT NULL REFERENCES accounts(account),
 prior_unsafe_epoch INTEGER NOT NULL CHECK(prior_unsafe_epoch>=0),
 prior_unsafe_reason TEXT NOT NULL CHECK(prior_unsafe_reason IN ('drain_failed','unclean_shutdown')),
 prior_archive_revision INTEGER NOT NULL CHECK(prior_archive_revision>=0),
 archive_revision INTEGER NOT NULL CHECK(archive_revision>prior_archive_revision),
 observation_sha256 TEXT NOT NULL DEFAULT '' CHECK(observation_sha256='' OR length(observation_sha256)=64),
 unsafe_sha256 TEXT NOT NULL DEFAULT '' CHECK(unsafe_sha256='' OR length(unsafe_sha256)=64),
 observation_file_identity TEXT NOT NULL DEFAULT '' CHECK(length(observation_file_identity)<=256),
 unsafe_file_identity TEXT NOT NULL DEFAULT '' CHECK(length(unsafe_file_identity)<=256),
 phase TEXT NOT NULL CHECK(phase IN ('prepared','complete')),
 created_at INTEGER NOT NULL,
 completed_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX snapshot_recovery_one_prepared ON snapshot_recovery_audit(phase) WHERE phase='prepared';
