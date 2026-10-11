-- Rebuild only the association parent. Existing child foreign keys use
-- NO ACTION; defer their checks until the replacement has the same name.
-- No media file, extraction result, reservation, or client grant is removed.
PRAGMA defer_foreign_keys=ON;
CREATE TABLE extension_media_associations_document_upgrade(
 id TEXT PRIMARY KEY,account TEXT NOT NULL REFERENCES accounts(account),message_id TEXT NOT NULL REFERENCES messages(id),
 local_revision INTEGER NOT NULL,policy_generation INTEGER NOT NULL,content_hash TEXT NOT NULL,source_expires_at INTEGER NOT NULL DEFAULT 0,
 ordinal INTEGER NOT NULL,kind TEXT NOT NULL CHECK(kind IN ('image','voice','document')),protocol_reference BLOB NOT NULL,
 mime_type TEXT NOT NULL DEFAULT '',declared_bytes INTEGER NOT NULL CHECK(declared_bytes>=0),view_once INTEGER NOT NULL DEFAULT 0,
 availability TEXT NOT NULL,active INTEGER NOT NULL DEFAULT 1,artifact_id TEXT NOT NULL DEFAULT '',
 last_original_sha256 TEXT NOT NULL DEFAULT '' CHECK(last_original_sha256='' OR length(last_original_sha256)=64),
 filename TEXT NOT NULL DEFAULT '' CHECK(typeof(filename)='text' AND length(CAST(filename AS BLOB))<=255 AND instr(filename,char(0))=0),
 UNIQUE(message_id,local_revision,ordinal)
);
INSERT INTO extension_media_associations_document_upgrade(
 id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,ordinal,kind,protocol_reference,
 mime_type,declared_bytes,view_once,availability,active,artifact_id,last_original_sha256
)
SELECT id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,ordinal,kind,protocol_reference,
 mime_type,declared_bytes,view_once,availability,active,artifact_id,last_original_sha256 FROM extension_media_associations;
DROP TABLE extension_media_associations;
ALTER TABLE extension_media_associations_document_upgrade RENAME TO extension_media_associations;
CREATE INDEX extension_media_source ON extension_media_associations(message_id,active);
CREATE INDEX extension_media_artifact_links ON extension_media_associations(artifact_id,active);

-- Only pre-upgrade unsupported originals can be enriched once when the SDK
-- delivers a document under the exact same source identity and legacy hash.
-- This marker is evidence of eligibility, never permission to read or download.
CREATE TABLE extension_document_promotions(
 message_id TEXT PRIMARY KEY REFERENCES messages(id),account TEXT NOT NULL REFERENCES accounts(account),
 local_revision INTEGER NOT NULL CHECK(local_revision>0),protocol_revision INTEGER NOT NULL CHECK(protocol_revision>=0),
 content_hash TEXT NOT NULL CHECK(typeof(content_hash)='text' AND length(CAST(content_hash AS BLOB))=64 AND content_hash NOT GLOB '*[^0-9a-f]*'),
 media_identity TEXT NOT NULL CHECK(typeof(media_identity)='text' AND (media_identity='' OR (length(CAST(media_identity AS BLOB))=64 AND media_identity NOT GLOB '*[^0-9a-f]*')))
);
INSERT INTO extension_document_promotions(message_id,account,local_revision,protocol_revision,content_hash,media_identity)
SELECT m.id,m.account,m.local_revision,m.protocol_revision,m.content_hash,m.extension_media_identity FROM messages m
WHERE m.state='active' AND m.source_kind='original' AND m.body_kind='unsupported' AND m.body='' AND m.caption=''
 AND m.normalized_body='' AND m.normalized_caption='' AND m.edited=0 AND m.view_once=0 AND m.revision_barrier=''
 AND m.original_date_known=1 AND typeof(m.original_sent_at)='integer' AND m.original_sent_at>0
 AND typeof(m.local_revision)='integer' AND m.local_revision>0 AND typeof(m.protocol_revision)='integer' AND m.protocol_revision>=0
 AND typeof(m.content_hash)='text' AND length(CAST(m.content_hash AS BLOB))=64 AND m.content_hash NOT GLOB '*[^0-9a-f]*'
 AND typeof(m.extension_media_identity)='text' AND (m.extension_media_identity='' OR (length(CAST(m.extension_media_identity AS BLOB))=64 AND m.extension_media_identity NOT GLOB '*[^0-9a-f]*'))
 AND NOT EXISTS(SELECT 1 FROM extension_media_associations a WHERE a.message_id=m.id)
 AND NOT EXISTS(SELECT 1 FROM source_tombstones t WHERE t.account=m.account AND t.chat=m.source_chat AND t.sender=m.source_sender AND t.message=m.source_message AND t.from_me=m.from_me)
 AND NOT EXISTS(SELECT 1 FROM chat_tombstones t WHERE t.account=m.account AND t.chat=m.source_chat AND m.original_sent_at<=t.cutoff)
 AND NOT EXISTS(SELECT 1 FROM unresolved_deletions d WHERE d.account=m.account AND d.chat=m.source_chat AND d.message=m.source_message);
