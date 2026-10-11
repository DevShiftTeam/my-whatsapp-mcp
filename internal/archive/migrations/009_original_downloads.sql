-- Durable initial-original download scheduling; migrations001--008 remain immutable.
-- Scheduling metadata never grants source or physical-original authority.
CREATE TABLE extension_original_downloads (
 media_id TEXT PRIMARY KEY REFERENCES extension_media_associations(id),
 account TEXT NOT NULL REFERENCES accounts(account),
 message_id TEXT NOT NULL REFERENCES messages(id),
 local_revision INTEGER NOT NULL,
 policy_generation INTEGER NOT NULL,
 content_hash TEXT NOT NULL,
 source_expires_at INTEGER NOT NULL DEFAULT 0,
 protocol_sha256 TEXT NOT NULL DEFAULT '' CHECK(typeof(protocol_sha256)='text' AND (protocol_sha256='' OR (length(protocol_sha256)=64 AND length(CAST(protocol_sha256 AS BLOB))=64 AND protocol_sha256 NOT GLOB '*[^0-9a-f]*'))),
 descriptor_sha256 TEXT NOT NULL DEFAULT '' CHECK(typeof(descriptor_sha256)='text' AND (descriptor_sha256='' OR (length(descriptor_sha256)=64 AND length(CAST(descriptor_sha256 AS BLOB))=64 AND descriptor_sha256 NOT GLOB '*[^0-9a-f]*'))),
 legacy_source_media_identity TEXT NOT NULL DEFAULT '' CHECK(typeof(legacy_source_media_identity)='text' AND (legacy_source_media_identity='' OR (length(legacy_source_media_identity)=64 AND length(CAST(legacy_source_media_identity AS BLOB))=64 AND legacy_source_media_identity NOT GLOB '*[^0-9a-f]*'))),
 state TEXT NOT NULL CHECK(state IN ('pending','leased','complete','cancelled','failed')),
 attempts INTEGER NOT NULL DEFAULT 0 CHECK(typeof(attempts)='integer' AND attempts>=0 AND attempts<=8),
 next_due INTEGER NOT NULL DEFAULT 0,
 lease_id TEXT NOT NULL DEFAULT '' CHECK(typeof(lease_id)='text' AND length(CAST(lease_id AS BLOB))<=128),
 lease_until INTEGER NOT NULL DEFAULT 0,
 last_code TEXT NOT NULL DEFAULT '' CHECK(last_code IN ('','media_unavailable','media_source_changed','media_canceled','media_deadline_exceeded','waiting_for_disk_budget','media_cleanup_incomplete','media_invalid_admission','media_authentication_failed','media_limit_exceeded')),
 completed_sha256 TEXT NOT NULL DEFAULT '' CHECK(typeof(completed_sha256)='text' AND (completed_sha256='' OR (length(completed_sha256)=64 AND length(CAST(completed_sha256 AS BLOB))=64 AND completed_sha256 NOT GLOB '*[^0-9a-f]*')))
);
CREATE INDEX extension_original_downloads_due ON extension_original_downloads(account,state,next_due,media_id);
CREATE INDEX extension_original_downloads_identity ON extension_original_downloads(account,message_id,descriptor_sha256);

-- Metadata-only seed; no protocol body enters Go or another table.
-- A completed verified identity survives not_retained/evicting/inactive state.
-- Whole-set proof is byte-identity metadata, captured only from the matching
-- authoritative account/message/revision/hash/expiry row. Policy-generation
-- rebasing does not erase this evidence; Claim/Load/Finish must independently
-- require the exact current policy and permitted SourceRef before any grant.
-- A historical association never borrows a newer message's identity.
INSERT INTO extension_original_downloads (
 media_id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,
 legacy_source_media_identity,state,completed_sha256
)
SELECT a.id,a.account,a.message_id,a.local_revision,a.policy_generation,a.content_hash,a.source_expires_at,
 CASE WHEN m.account=a.account AND m.id=a.message_id AND m.local_revision=a.local_revision
  AND m.content_hash=a.content_hash AND m.expires_at=a.source_expires_at AND m.state='active' AND m.view_once=0
  AND typeof(m.extension_media_identity)='text' AND length(m.extension_media_identity)=64
  AND length(CAST(m.extension_media_identity AS BLOB))=64 AND m.extension_media_identity NOT GLOB '*[^0-9a-f]*'
 THEN m.extension_media_identity ELSE '' END,
 CASE WHEN typeof(a.last_original_sha256)='text' AND length(a.last_original_sha256)=64
  AND length(CAST(a.last_original_sha256 AS BLOB))=64 AND a.last_original_sha256 NOT GLOB '*[^0-9a-f]*'
 THEN 'complete' WHEN a.last_original_sha256<>'' THEN 'cancelled'
 WHEN a.active=1 AND a.view_once=0 THEN 'pending' ELSE 'cancelled' END,
 CASE WHEN typeof(a.last_original_sha256)='text' AND length(a.last_original_sha256)=64
  AND length(CAST(a.last_original_sha256 AS BLOB))=64 AND a.last_original_sha256 NOT GLOB '*[^0-9a-f]*'
 THEN a.last_original_sha256 ELSE '' END
FROM extension_media_associations a LEFT JOIN messages m ON m.id=a.message_id AND m.account=a.account;
