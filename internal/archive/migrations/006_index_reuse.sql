ALTER TABLE extension_documents ADD COLUMN origin_document_id TEXT NOT NULL DEFAULT '';
ALTER TABLE extension_documents ADD COLUMN origin_start_byte INTEGER NOT NULL DEFAULT 0;
ALTER TABLE extension_documents ADD COLUMN origin_end_byte INTEGER NOT NULL DEFAULT 0;
ALTER TABLE extension_documents ADD COLUMN embedding_input INTEGER NOT NULL DEFAULT 0 CHECK(embedding_input IN (0,1));
CREATE INDEX extension_document_origin_generation ON extension_documents(generation,origin_document_id,embedding_input);
-- Preserve existing extraction roots without reading their bodies into Go.
-- Private token-zero roots stay unavailable to lexical/vector/hydration reads.
UPDATE extension_documents SET origin_document_id=id,origin_start_byte=0,origin_end_byte=length(CAST(text AS BLOB)),embedding_input=CASE WHEN passage_tokens=0 THEN 1 ELSE 0 END WHERE kind IN ('ocr','caption','transcript');
DELETE FROM extension_document_fts WHERE rowid IN (SELECT rowid FROM extension_documents WHERE embedding_input=1);
