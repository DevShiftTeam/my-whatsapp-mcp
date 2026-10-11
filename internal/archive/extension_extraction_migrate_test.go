//go:build sqlite_fts5

package archive

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtensionIndependentUpgradeCopiesOnlyVerifiedUnambiguousMediaIdentity(t *testing.T) {
	for _, kind := range []string{"linked_shared", "unique_history", "ambiguous_history", "wrong_revision", "wrong_format", "uppercase_hash", "inactive"} {
		t.Run(kind, func(t *testing.T) {
			db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "legacy.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			if _, err = db.Exec("PRAGMA foreign_keys=ON"); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"001_archive.sql", "002_indexing.sql", "003_media.sql", "004_credentials.sql", "005_media_identity.sql", "006_index_reuse.sql", "007_fts_secure_delete.sql"} {
				body, readErr := migrationFiles.ReadFile("migrations/" + name)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if _, err = db.Exec(string(body)); err != nil {
					t.Fatal(err)
				}
			}
			for _, query := range []string{
				"INSERT INTO accounts(account) VALUES('account')",
				"INSERT INTO chats(id,account,source_chat) VALUES('chat','account','chat')",
				"INSERT INTO identities(id,account,source_identity) VALUES('sender','account','sender')",
				"INSERT INTO messages(id,account,chat_id,sender_id,source_chat,source_sender,source_message,from_me,state,source_kind) VALUES('source','account','chat','sender','chat','sender','source',0,'active','original')",
				"INSERT INTO messages(id,account,chat_id,sender_id,source_chat,source_sender,source_message,from_me,state,source_kind) VALUES('other','account','chat','sender','chat','sender','other',0,'active','original')",
			} {
				if _, err = db.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			hash := strings.Repeat("a", 64)
			active := 1
			if kind == "inactive" {
				active = 0
			}
			for ordinal, id := range []string{"media", "owner"} {
				if _, err = db.Exec(`INSERT INTO extension_media_associations(id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,ordinal,kind,protocol_reference,declared_bytes,availability,active)
 VALUES(?,'account','source',2,3,?,99,?,'image',X'01',4,'not_retained',?)`, id, hash, ordinal, active); err != nil {
					t.Fatal(err)
				}
			}
			insertArtifact := func(id, media, message, sha, format string, revision int) {
				t.Helper()
				if _, err = db.Exec(`INSERT INTO extension_media_artifacts(id,reservation_id,media_id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,budget,maximum_bytes,charged_bytes,lease_id,lease_until,state,sha256,bytes,actual_format,cleanup_ack,created_at,updated_at)
 VALUES(?,?,?,'account',?,?,3,?,99,'originals',4,0,'lease',0,'evicting',?,4,?,1,1,1)`, id, id, media, message, revision, hash, sha, format); err != nil {
					t.Fatal(err)
				}
			}
			want := ""
			switch kind {
			case "linked_shared":
				insertArtifact("artifact", "owner", "other", hash, "png", 17)
				if _, err = db.Exec("UPDATE extension_media_associations SET artifact_id='artifact' WHERE id='media'"); err != nil {
					t.Fatal(err)
				}
				want = hash
			case "unique_history":
				insertArtifact("artifact-a", "media", "source", hash, "png", 2)
				insertArtifact("artifact-b", "media", "source", hash, "png", 2)
				want = hash
			case "ambiguous_history":
				insertArtifact("artifact-a", "media", "source", hash, "png", 2)
				insertArtifact("artifact-b", "media", "source", strings.Repeat("b", 64), "png", 2)
			case "wrong_revision":
				insertArtifact("artifact", "media", "source", hash, "png", 1)
			case "wrong_format":
				insertArtifact("artifact", "media", "source", hash, "ogg", 2)
			case "uppercase_hash":
				insertArtifact("artifact", "media", "source", strings.Repeat("A", 64), "png", 2)
			case "inactive":
				insertArtifact("artifact", "media", "source", hash, "png", 2)
			}
			body, err := migrationFiles.ReadFile("migrations/008_publication_extraction.sql")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(string(body)); err != nil {
				t.Fatal(err)
			}
			var got string
			if err = db.QueryRow("SELECT last_original_sha256 FROM extension_media_associations WHERE id='media'").Scan(&got); err != nil || got != want {
				t.Fatalf("upgrade changed verified media authority: got=%q want=%q err=%v", got, want, err)
			}
			for _, table := range []string{"extension_roots", "extension_extraction_jobs", "extension_window_coverage"} {
				var count int
				if err = db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("upgrade fabricated %s proof: count=%d err=%v", table, count, err)
				}
			}
		})
	}
}
