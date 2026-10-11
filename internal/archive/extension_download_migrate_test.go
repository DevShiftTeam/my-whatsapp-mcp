//go:build sqlite_fts5

package archive

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func originalDownloadLegacyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_archive.sql", "002_indexing.sql", "003_media.sql", "004_credentials.sql", "005_media_identity.sql", "006_index_reuse.sql", "007_fts_secure_delete.sql", "008_publication_extraction.sql"} {
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
		"INSERT INTO archive_policy(id,generation,date_mode,import_start,chat_mode) VALUES(1,9,'all',0,'all')",
	} {
		if _, err = db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func applyOriginalDownloadMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	body, err := migrationFiles.ReadFile("migrations/009_original_downloads.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
}

func TestOriginalDownloadMigrationSeedsCompletionWithoutBorrowingNewerIdentity(t *testing.T) {
	for _, kind := range []string{"not_retained", "inactive", "expired", "view_once", "policy_rebase", "wrong_revision", "wrong_hash", "wrong_expiry", "nul_identity", "blob_identity", "malformed_marker", "pending"} {
		t.Run(kind, func(t *testing.T) {
			db := originalDownloadLegacyDB(t)
			hash, setIdentity, marker := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
			state, active, viewOnce := "active", 1, 0
			associationRevision, associationExpiry := 2, 99
			associationHash := hash
			var identity any = setIdentity
			wantIdentity, wantMarker, wantState := setIdentity, marker, "complete"
			switch kind {
			case "inactive":
				active = 0
			case "expired":
				state, wantIdentity = "expired", ""
			case "view_once":
				viewOnce, wantIdentity = 1, ""
			case "wrong_revision":
				associationRevision, wantIdentity = 1, ""
			case "wrong_hash":
				associationHash, wantIdentity = strings.Repeat("d", 64), ""
			case "wrong_expiry":
				associationExpiry, wantIdentity = 100, ""
			case "nul_identity":
				identity, wantIdentity = setIdentity+"\x00tail", ""
			case "blob_identity":
				identity, wantIdentity = []byte(setIdentity), ""
			case "malformed_marker":
				marker, wantMarker, wantState = strings.Repeat("C", 64), "", "cancelled"
			case "pending":
				marker, wantMarker, wantState = "", "", "pending"
			}
			if _, err := db.Exec(`INSERT INTO messages(id,account,chat_id,sender_id,source_chat,source_sender,source_message,from_me,state,source_kind,local_revision,content_hash,expires_at,view_once,extension_media_identity)
 VALUES('source','account','chat','sender','chat','sender','source',0,?,'original',2,?,99,?,?)`, state, hash, viewOnce, identity); err != nil {
				t.Fatal(err)
			}
			// Policy generation3 is deliberately older than current generation9.
			// This is byte-identity evidence, never current admission authority.
			if _, err := db.Exec(`INSERT INTO extension_media_associations(id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,ordinal,kind,protocol_reference,declared_bytes,availability,active,view_once,last_original_sha256)
 VALUES('media','account','source',?,3,?,?,0,'image',X'01',4,'not_retained',?,?,?)`, associationRevision, associationHash, associationExpiry, active, viewOnce, marker); err != nil {
				t.Fatal(err)
			}
			applyOriginalDownloadMigration(t, db)
			var gotState, gotMarker, gotIdentity, referenceDigest, descriptorDigest string
			if err := db.QueryRow(`SELECT state,completed_sha256,legacy_source_media_identity,protocol_sha256,descriptor_sha256 FROM extension_original_downloads WHERE media_id='media'`).Scan(&gotState, &gotMarker, &gotIdentity, &referenceDigest, &descriptorDigest); err != nil {
				t.Fatal(err)
			}
			if gotState != wantState || gotMarker != wantMarker || gotIdentity != wantIdentity || referenceDigest != "" || descriptorDigest != "" {
				t.Fatalf("seed fabricated or erased identity: state=%q marker=%q identity=%q reference=%q descriptor=%q", gotState, gotMarker, gotIdentity, referenceDigest, descriptorDigest)
			}
			for _, table := range []string{"extension_media_artifacts", "extension_extraction_jobs", "extension_roots", "extension_documents"} {
				var count int
				if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("seed fabricated %s authority: count=%d err=%v", table, count, err)
				}
			}
		})
	}
}

func TestOriginalDownloadMigrationRejectsMalformedDigestAndStoresNoProtocolCopy(t *testing.T) {
	db := originalDownloadLegacyDB(t)
	if _, err := db.Exec(`INSERT INTO messages(id,account,chat_id,sender_id,source_chat,source_sender,source_message,from_me,state,source_kind) VALUES('source','account','chat','sender','chat','sender','source',0,'active','original')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO extension_media_associations(id,account,message_id,local_revision,policy_generation,content_hash,ordinal,kind,protocol_reference,declared_bytes,availability) VALUES('media','account','source',1,9,'',0,'image',X'0102',4,'not_retained')`); err != nil {
		t.Fatal(err)
	}
	applyOriginalDownloadMigration(t, db)
	for _, column := range []string{"protocol_sha256", "descriptor_sha256", "legacy_source_media_identity", "completed_sha256"} {
		for _, valid := range []string{strings.Repeat("a", 64), ""} {
			if _, err := db.Exec("UPDATE extension_original_downloads SET "+column+"=? WHERE media_id='media'", valid); err != nil {
				t.Fatalf("%s rejected a valid digest or empty sentinel: %v", column, err)
			}
		}
		for _, malformed := range []any{strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("a", 64) + "\x00tail", []byte(strings.Repeat("a", 64))} {
			if _, err := db.Exec("UPDATE extension_original_downloads SET "+column+"=? WHERE media_id='media'", malformed); err == nil {
				t.Fatalf("%s accepted malformed digest", column)
			}
		}
	}
	rows, err := db.Query("PRAGMA table_info(extension_original_downloads)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	allowed := map[string]bool{
		"media_id": true, "account": true, "message_id": true,
		"local_revision": true, "policy_generation": true, "content_hash": true,
		"source_expires_at": true, "protocol_sha256": true, "descriptor_sha256": true,
		"legacy_source_media_identity": true, "state": true, "attempts": true,
		"next_due": true, "lease_id": true, "lease_until": true,
		"last_code": true, "completed_sha256": true,
	}
	for rows.Next() {
		var index, notNull, primary int
		var name, typ string
		var defaultValue any
		if err = rows.Scan(&index, &name, &typ, &notNull, &defaultValue, &primary); err != nil {
			t.Fatal(err)
		}
		if !allowed[name] {
			t.Fatalf("scheduling table contains an unapproved non-metadata column: %s", name)
		}
		delete(allowed, name)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(allowed) != 0 {
		t.Fatalf("scheduling metadata columns are missing: %v", allowed)
	}
}
