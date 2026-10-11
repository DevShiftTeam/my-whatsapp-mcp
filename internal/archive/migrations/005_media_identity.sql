ALTER TABLE messages ADD COLUMN extension_media_identity TEXT NOT NULL DEFAULT '' CHECK(extension_media_identity='' OR length(extension_media_identity)=64);
