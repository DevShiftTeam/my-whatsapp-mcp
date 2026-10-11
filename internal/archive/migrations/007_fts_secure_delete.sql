-- Existing migration checksums and authoritative source rows stay unchanged.
-- Rebuild removes historical FTS delete keys under the existing core secure_delete.
INSERT INTO message_fts(message_fts,rank) VALUES('secure-delete',1);
INSERT INTO message_fts(message_fts) VALUES('rebuild');
INSERT INTO extension_document_fts(extension_document_fts,rank) VALUES('secure-delete',1);
INSERT INTO extension_document_fts(extension_document_fts) VALUES('rebuild');
