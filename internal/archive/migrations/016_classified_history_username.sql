-- Preserve ordinary recovery audits and preserve both exact reviewed owner classifications.
DROP INDEX snapshot_recovery_one_prepared;
ALTER TABLE snapshot_recovery_audit RENAME TO prior_snapshot_recovery_audit;
CREATE TABLE snapshot_recovery_audit (
 recovery_epoch INTEGER PRIMARY KEY CHECK(recovery_epoch>0),
 account TEXT NOT NULL REFERENCES accounts(account),
 prior_unsafe_epoch INTEGER NOT NULL CHECK(prior_unsafe_epoch>=0),
 prior_unsafe_reason TEXT NOT NULL CHECK(prior_unsafe_reason IN ('drain_failed','unclean_shutdown','history_corrupt')),
 prior_archive_revision INTEGER NOT NULL CHECK(prior_archive_revision>=0),
 archive_revision INTEGER NOT NULL CHECK(archive_revision>prior_archive_revision),
 observation_sha256 TEXT NOT NULL DEFAULT '' CHECK(observation_sha256='' OR length(observation_sha256)=64),
 unsafe_sha256 TEXT NOT NULL DEFAULT '' CHECK(unsafe_sha256='' OR length(unsafe_sha256)=64),
 observation_file_identity TEXT NOT NULL DEFAULT '' CHECK(length(observation_file_identity)<=256),
 unsafe_file_identity TEXT NOT NULL DEFAULT '' CHECK(length(unsafe_file_identity)<=256),
 phase TEXT NOT NULL CHECK(phase IN ('prepared','complete')),
 created_at INTEGER NOT NULL,
 completed_at INTEGER NOT NULL DEFAULT 0,
 classification TEXT NOT NULL DEFAULT '',
 witness_json TEXT NOT NULL DEFAULT '' CHECK(length(CAST(witness_json AS BLOB))<=8192),
 witness_sha256 TEXT NOT NULL DEFAULT '' CHECK(witness_sha256='' OR length(witness_sha256)=64),
 CHECK((prior_unsafe_reason='history_corrupt' AND classification IN ('v1_group_participant_change_number','v1_noncontent_history_notice') AND witness_json<>'' AND length(witness_sha256)=64) OR (prior_unsafe_reason IN ('drain_failed','unclean_shutdown') AND classification='' AND witness_json='' AND witness_sha256=''))
);
CREATE UNIQUE INDEX snapshot_recovery_one_prepared ON snapshot_recovery_audit(phase) WHERE phase='prepared';
INSERT INTO snapshot_recovery_audit(recovery_epoch,account,prior_unsafe_epoch,prior_unsafe_reason,prior_archive_revision,archive_revision,observation_sha256,unsafe_sha256,observation_file_identity,unsafe_file_identity,phase,created_at,completed_at,classification,witness_json,witness_sha256) SELECT recovery_epoch,account,prior_unsafe_epoch,prior_unsafe_reason,prior_archive_revision,archive_revision,observation_sha256,unsafe_sha256,observation_file_identity,unsafe_file_identity,phase,created_at,completed_at,classification,witness_json,witness_sha256 FROM prior_snapshot_recovery_audit;
DROP TABLE prior_snapshot_recovery_audit;
