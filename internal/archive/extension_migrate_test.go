//go:build sqlite_fts5

package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExtensionMigrationsAreOrderedChecksummedAndReopen(t *testing.T) {
	s := testArchive(t)
	rows, err := s.readDB.Query("SELECT version,checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		i++
		var v int
		var hash string
		if err = rows.Scan(&v, &hash); err != nil {
			t.Fatal(err)
		}
		if v != i || len(hash) != 64 {
			t.Fatalf("migration order/checksum: %d %d", i, v)
		}
	}
	if i != 14 {
		t.Fatalf("expected 14 ordered migrations, got %d", i)
	}
	body, err := migrationFiles.ReadFile("migrations/001_archive.sql")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	var stored string
	if err = s.readDB.QueryRow("SELECT checksum FROM schema_migrations WHERE version=1").Scan(&stored); err != nil || stored != hex.EncodeToString(sum[:]) {
		t.Fatalf("001 changed: %v", err)
	}
	dir := s.privateDir
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
}
func TestExtensionMigrationGapAndChangedChecksumReject(t *testing.T) {
	for _, name := range []string{"gap", "checksum", "future"} {
		t.Run(name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s, err := Open(context.Background(), dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			query := "DELETE FROM schema_migrations WHERE version=2"
			expected := ErrSchemaChanged
			if name == "checksum" {
				query = "UPDATE schema_migrations SET checksum='changed' WHERE version=3"
			}
			if name == "future" {
				query = "INSERT INTO schema_migrations(version,checksum) SELECT max(version)+1,'future' FROM schema_migrations"
				expected = ErrSchemaTooNew
			}
			if _, err = s.writeDB.Exec(query); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err = Open(context.Background(), dir, Options{}); !errors.Is(err, expected) {
				t.Fatalf("%s: %v", name, err)
			}
		})
	}
}

func TestExtensionUpgradePreservesExistingV1SourcesAndCursorKey(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "archive.sqlite")
	if err = os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("PRAGMA secure_delete=ON"); err != nil {
		t.Fatal(err)
	}
	legacy, err := migrationFiles.ReadFile("migrations/001_archive.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(legacy)); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(legacy)
	if _, err = db.Exec("CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,checksum TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO schema_migrations VALUES(1,?)", hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x1e}, 32)
	if _, err = db.Exec("INSERT INTO archive_meta VALUES(1,44,9,?,?)", LexicalNormalizationVersion, key); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO accounts VALUES('account')",
		"INSERT INTO account_safety(account) VALUES('account')",
		"INSERT INTO identities(id,account,source_identity) VALUES('legacy-sender','account','sender')",
		"INSERT INTO chats(id,account,source_chat) VALUES('legacy-chat','account','chat')",
		"INSERT INTO archive_policy VALUES(1,7,'all_available',0,'all',0)",
	} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	e := event("legacy", "legacy preserved", 3, EventOriginal)
	e.Caption = "old caption"
	hash := hashEvent(e)
	if _, err = db.Exec(`INSERT INTO messages(id,account,chat_id,sender_id,source_chat,source_sender,source_message,from_me,original_sent_at,original_date_known,protocol_revision,local_revision,state,source_kind,body,caption,normalized_body,normalized_caption,body_kind,content_hash) VALUES('legacy-message','account','legacy-chat','legacy-sender','chat','sender','legacy',0,?,1,3,12,'active','original',?,?,?,?, 'text',?)`, unixms(e.OriginalSentAt), e.Body, e.Caption, NormalizeLexical(e.Body), NormalizeLexical(e.Caption), hash); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO message_fts(rowid,normalized_body,normalized_caption) SELECT rowid,normalized_body,normalized_caption FROM messages"); err != nil {
		t.Fatal(err)
	}
	const deletedLexicalCanary = "historicaldeletedlexicalcanaryqzjxv"
	if _, err = db.Exec("INSERT INTO message_fts(rowid,normalized_body,normalized_caption) VALUES(999,?,'')", deletedLexicalCanary); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("DELETE FROM message_fts WHERE rowid=999"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	beforeUpgrade, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(beforeUpgrade, []byte(deletedLexicalCanary)) {
		t.Fatalf("legacy FTS deletion-key fixture absent: %v", err)
	}
	s, err := Open(context.Background(), dir, Options{Now: func() time.Time { return e.OriginalSentAt.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ref, err := s.CurrentSource(context.Background(), e.Key)
	if err != nil || ref.ContentHash != hash || ref.Version.Revision != 12 || ref.Version.PolicyGeneration != 7 || !bytes.Equal(s.cursorKey, key) {
		t.Fatalf("v1 source identity/hash/cursor changed during upgrade: revision=%d policy=%d err=%v", ref.Version.Revision, ref.Version.PolicyGeneration, err)
	}
	var body, caption string
	var revision ArchiveRevision
	if err = s.readDB.QueryRow("SELECT body,caption FROM messages WHERE id='legacy-message'").Scan(&body, &caption); err != nil || body != e.Body || caption != e.Caption {
		t.Fatal("upgrade changed authorized original fields")
	}
	if err = s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&revision); err != nil || revision != 44 {
		t.Fatalf("upgrade changed source revision: %d %v", revision, err)
	}
	if _, err = s.writeDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	afterUpgrade, err := os.ReadFile(path)
	if err != nil || bytes.Contains(afterUpgrade, []byte(deletedLexicalCanary)) {
		t.Fatalf("upgrade retained historical FTS deletion key: %v", err)
	}
}

func TestExtensionReuseUpgradePreservesDerivedRootsAndHidesUntokenizedInputs(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_archive.sql", "002_indexing.sql", "003_media.sql", "004_credentials.sql"} {
		body, readErr := migrationFiles.ReadFile("migrations/" + name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, err = db.Exec(string(body)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec("INSERT INTO extension_generations VALUES('old-generation','{}','active',1)"); err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		id     string
		tokens int
	}{{"visible-root", 4}, {"untokenized-root", 0}} {
		text := "immutable root ישראל " + input.id
		provenance := fmt.Sprintf(`{"Kind":"ocr","ProfileID":"old-extractor","MediaID":"%s"}`, input.id)
		if _, err = db.Exec("INSERT INTO extension_documents(id,generation,kind,text,normalized_text,provenance_json,spans_json,passage_tokens,content_hash,source_count) VALUES(?,'old-generation','ocr',?,?,?,'null',?,'unchanged-work-digest',1)", input.id, text, NormalizeLexical(text), provenance, input.tokens); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec("INSERT INTO extension_document_fts(rowid,normalized_text) SELECT rowid,normalized_text FROM extension_documents"); err != nil {
		t.Fatal(err)
	}
	migration, err := migrationFiles.ReadFile("migrations/006_index_reuse.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"visible-root", "untokenized-root"} {
		var origin, text, provenance, digest string
		var start, end, hidden, lexical int
		if err = db.QueryRow("SELECT origin_document_id,origin_start_byte,origin_end_byte,embedding_input,text,provenance_json,content_hash FROM extension_documents WHERE id=?", id).Scan(&origin, &start, &end, &hidden, &text, &provenance, &digest); err != nil {
			t.Fatal(err)
		}
		wantHidden := 0
		if id == "untokenized-root" {
			wantHidden = 1
		}
		wantText := "immutable root ישראל " + id
		wantProvenance := fmt.Sprintf(`{"Kind":"ocr","ProfileID":"old-extractor","MediaID":"%s"}`, id)
		if origin != id || start != 0 || end != len(wantText) || hidden != wantHidden || text != wantText || provenance != wantProvenance || digest != "unchanged-work-digest" {
			t.Fatalf("reuse upgrade lost root identity/provenance: id=%s origin=%s range=%d:%d hidden=%d", id, origin, start, end, hidden)
		}
		if err = db.QueryRow("SELECT count(*) FROM extension_document_fts WHERE rowid=(SELECT rowid FROM extension_documents WHERE id=?)", id).Scan(&lexical); err != nil || lexical != 1-wantHidden {
			t.Fatalf("untokenized root retained lexical authority or visible root was lost: id=%s lexical=%d err=%v", id, lexical, err)
		}
	}
}
