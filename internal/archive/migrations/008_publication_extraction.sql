-- Independent authoritative extraction roots never depend on an encoder generation.
ALTER TABLE extension_media_associations ADD COLUMN last_original_sha256 TEXT NOT NULL DEFAULT '' CHECK(last_original_sha256='' OR length(last_original_sha256)=64);
-- Copy only an already verified digest. Ambiguous historical digests remain
-- private until a new authorized publication establishes current identity.
UPDATE extension_media_associations AS m SET last_original_sha256=COALESCE(
 (SELECT a.sha256 FROM extension_media_artifacts a WHERE a.id=m.artifact_id AND a.budget='originals' AND a.state IN ('retained','evicting')
  AND a.bytes>0 AND a.bytes<=a.maximum_bytes AND length(a.sha256)=64 AND a.sha256 NOT GLOB '*[^0-9a-f]*'
  AND ((m.kind='image' AND a.actual_format IN ('png','jpeg','webp')) OR (m.kind='voice' AND a.actual_format IN ('ogg','opus','wav','mp3','flac','m4a')))),
 (SELECT MIN(a.sha256) FROM extension_media_artifacts a WHERE a.media_id=m.id AND a.account=m.account AND a.message_id=m.message_id
  AND a.local_revision=m.local_revision AND a.policy_generation=m.policy_generation AND a.content_hash=m.content_hash AND a.source_expires_at=m.source_expires_at
  AND a.budget='originals' AND a.state IN ('retained','evicting') AND a.bytes>0 AND a.bytes<=a.maximum_bytes
  AND length(a.sha256)=64 AND a.sha256 NOT GLOB '*[^0-9a-f]*'
  AND ((m.kind='image' AND a.actual_format IN ('png','jpeg','webp')) OR (m.kind='voice' AND a.actual_format IN ('ogg','opus','wav','mp3','flac','m4a')))
  HAVING COUNT(DISTINCT a.sha256)=1),'') WHERE m.active=1 AND m.view_once=0;
CREATE TABLE extension_extraction_versions(
 id TEXT PRIMARY KEY,version_json TEXT NOT NULL CHECK(length(CAST(version_json AS BLOB))<=4096),version_hash TEXT NOT NULL UNIQUE CHECK(length(version_hash)=64)
);
CREATE TABLE extension_extraction_jobs(
 id TEXT PRIMARY KEY,account TEXT NOT NULL REFERENCES accounts(account),version_id TEXT NOT NULL REFERENCES extension_extraction_versions(id),
 kind TEXT NOT NULL CHECK(kind IN ('ocr','caption','transcription')),media_id TEXT NOT NULL REFERENCES extension_media_associations(id),artifact_id TEXT NOT NULL,
 original_sha256 TEXT NOT NULL CHECK(length(original_sha256)=64),message_id TEXT NOT NULL REFERENCES messages(id),local_revision INTEGER NOT NULL CHECK(local_revision>0),
 policy_generation INTEGER NOT NULL CHECK(policy_generation>0),content_hash TEXT NOT NULL CHECK(length(content_hash)=64),source_expires_at INTEGER NOT NULL DEFAULT 0,
 work_hash TEXT NOT NULL CHECK(length(work_hash)=64),state TEXT NOT NULL CHECK(state IN ('pending','leased','complete','failed','cancelled')),
 attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0),retry_at INTEGER NOT NULL DEFAULT 0,lease_id TEXT NOT NULL DEFAULT '',lease_until INTEGER NOT NULL DEFAULT 0,
 last_code TEXT NOT NULL DEFAULT '',input_pinned INTEGER NOT NULL DEFAULT 0 CHECK(input_pinned IN (0,1)),
 outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','text','no_text','silent')),completion_digest TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,
 UNIQUE(version_id,media_id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,original_sha256)
);
CREATE INDEX extension_extraction_claim ON extension_extraction_jobs(account,version_id,state,retry_at,lease_until);
CREATE INDEX extension_extraction_input_pins ON extension_extraction_jobs(artifact_id,input_pinned);
CREATE INDEX extension_extraction_recovery ON extension_extraction_jobs(account,input_pinned,id);
CREATE INDEX extension_extraction_source ON extension_extraction_jobs(message_id,state);
CREATE TABLE extension_roots(
 id TEXT PRIMARY KEY,extraction_job_id TEXT REFERENCES extension_extraction_jobs(id),legacy_document_id TEXT NOT NULL DEFAULT '',
 kind TEXT NOT NULL CHECK(kind IN ('ocr','caption','transcript')),text TEXT NOT NULL CHECK(length(CAST(text AS BLOB)) BETWEEN 1 AND 65536),
 text_sha256 TEXT NOT NULL CHECK(length(text_sha256)=64),text_bytes INTEGER NOT NULL CHECK(text_bytes BETWEEN 1 AND 65536 AND text_bytes=length(CAST(text AS BLOB))),
 media_id TEXT NOT NULL REFERENCES extension_media_associations(id),provenance_json TEXT NOT NULL CHECK(length(CAST(provenance_json AS BLOB))<=4096),
 source_count INTEGER NOT NULL CHECK(source_count BETWEEN 1 AND 2048),created_at INTEGER NOT NULL,
 CHECK((extraction_job_id IS NOT NULL AND legacy_document_id='') OR (extraction_job_id IS NULL AND legacy_document_id<>''))
);
CREATE UNIQUE INDEX extension_root_legacy_document ON extension_roots(legacy_document_id) WHERE legacy_document_id<>'';
CREATE INDEX extension_root_media ON extension_roots(media_id);
CREATE TABLE extension_root_sources(
 root_id TEXT NOT NULL REFERENCES extension_roots(id) ON DELETE CASCADE,account TEXT NOT NULL,message_id TEXT NOT NULL REFERENCES messages(id),
 local_revision INTEGER NOT NULL CHECK(local_revision>0),policy_generation INTEGER NOT NULL CHECK(policy_generation>0),
 content_hash TEXT NOT NULL CHECK(length(content_hash)=64),expires_at INTEGER NOT NULL DEFAULT 0,ordinal INTEGER NOT NULL CHECK(ordinal>=0),
 PRIMARY KEY(root_id,message_id),UNIQUE(root_id,ordinal)
);
CREATE INDEX extension_root_source_message ON extension_root_sources(message_id,root_id);
CREATE TABLE extension_extraction_outputs(
 job_id TEXT NOT NULL REFERENCES extension_extraction_jobs(id) ON DELETE CASCADE,root_id TEXT NOT NULL REFERENCES extension_roots(id) ON DELETE CASCADE,
 ordinal INTEGER NOT NULL CHECK(ordinal>=0),PRIMARY KEY(job_id,ordinal),UNIQUE(root_id)
);
CREATE VIRTUAL TABLE extension_root_fts USING fts5(root_id UNINDEXED,text,tokenize='unicode61 remove_diacritics 2');
INSERT INTO extension_root_fts(extension_root_fts,rank) VALUES('secure-delete',1);
CREATE TABLE extension_job_origins(
 job_id TEXT PRIMARY KEY REFERENCES extension_jobs(id) ON DELETE CASCADE,root_id TEXT NOT NULL REFERENCES extension_roots(id) ON DELETE CASCADE,
 origin_hash TEXT NOT NULL DEFAULT ''
);
CREATE INDEX extension_job_origin_root ON extension_job_origins(root_id,job_id);
CREATE INDEX extension_job_document_reverse ON extension_job_documents(document_id,job_id);
-- The final batch receipt survives work acknowledgement for exact retry recovery.
CREATE TABLE extension_publications(
 kind TEXT NOT NULL CHECK(kind IN ('window','derived')),work_id TEXT NOT NULL,account TEXT NOT NULL REFERENCES accounts(account),generation TEXT NOT NULL REFERENCES extension_generations(id),
 identity_hash TEXT NOT NULL,lease_id TEXT NOT NULL,lease_until INTEGER NOT NULL,cursor TEXT NOT NULL DEFAULT '',last_cursor TEXT NOT NULL DEFAULT '',
 last_digest TEXT NOT NULL DEFAULT '',last_next TEXT NOT NULL DEFAULT '',last_complete INTEGER NOT NULL DEFAULT 0 CHECK(last_complete IN (0,1)),
 prepared INTEGER NOT NULL DEFAULT 0 CHECK(prepared IN (0,1)),cleanup_after TEXT NOT NULL DEFAULT '',source_ordinal INTEGER NOT NULL DEFAULT 0 CHECK(source_ordinal>=0),
 field_ordinal INTEGER NOT NULL DEFAULT 0 CHECK(field_ordinal>=0),start_byte INTEGER NOT NULL DEFAULT 0 CHECK(start_byte>=0),loaded_end INTEGER NOT NULL DEFAULT 0 CHECK(loaded_end>=0),
 complete INTEGER NOT NULL DEFAULT 0 CHECK(complete IN (0,1)),PRIMARY KEY(kind,work_id)
);
CREATE TABLE extension_publication_sources(
 kind TEXT NOT NULL,work_id TEXT NOT NULL,account TEXT NOT NULL,message_id TEXT NOT NULL REFERENCES messages(id),local_revision INTEGER NOT NULL,
 policy_generation INTEGER NOT NULL,expires_at INTEGER NOT NULL DEFAULT 0,content_hash TEXT NOT NULL,ordinal INTEGER NOT NULL CHECK(ordinal>=0),
 PRIMARY KEY(kind,work_id,message_id),UNIQUE(kind,work_id,ordinal),FOREIGN KEY(kind,work_id) REFERENCES extension_publications(kind,work_id) ON DELETE CASCADE
);
CREATE INDEX extension_publication_source_message ON extension_publication_sources(message_id,kind,work_id);
CREATE TABLE extension_window_coverage(
 generation TEXT NOT NULL REFERENCES extension_generations(id),account TEXT NOT NULL,message_id TEXT NOT NULL REFERENCES messages(id),local_revision INTEGER NOT NULL,
 policy_generation INTEGER NOT NULL,expires_at INTEGER NOT NULL DEFAULT 0,content_hash TEXT NOT NULL,field TEXT NOT NULL CHECK(field IN ('body','caption')),
 bytes INTEGER NOT NULL CHECK(bytes>=0),PRIMARY KEY(generation,account,message_id,field)
);
-- One persisted authority governs every original reservation, including work
-- admitted before a later decrease. Changes never forgive physical charges.
CREATE TABLE extension_original_budget(
 id INTEGER PRIMARY KEY CHECK(id=1),maximum_bytes INTEGER NOT NULL CHECK(maximum_bytes BETWEEN 67108864 AND 1099511627776),
 revision INTEGER NOT NULL CHECK(revision>0)
);
INSERT INTO extension_original_budget(id,maximum_bytes,revision) VALUES(1,2147483648,1);
