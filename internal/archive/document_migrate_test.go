//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func documentLegacyDB(t *testing.T) *sql.DB {
	t.Helper()
	// A cancelled transaction may make database/sql discard its connection.
	// Keep the fixture on disk so reopening still observes the rollback.
	path := filepath.Join(t.TempDir(), "migration.sqlite")
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_foreign_keys=on"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for i, entry := range entries {
		if i >= 13 {
			break
		}
		body, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO accounts(account) VALUES('account')`,
		`INSERT INTO chats(id,account,source_chat) VALUES('chat','account','chat')`,
		`INSERT INTO identities(id,account,source_identity) VALUES('sender','account','sender')`,
		`INSERT INTO archive_policy(id,generation,date_mode,import_start,chat_mode) VALUES(1,9,'all_available',0,'all')`,
		`INSERT INTO archive_meta(id,archive_revision,index_generation,normalization_version,cursor_key) VALUES(1,41,1,'` + LexicalNormalizationVersion + `',zeroblob(32))`,
		`INSERT INTO credentials(id,digest,account,label,principal,revision,all_chats,created_at) VALUES('11111111111111111111111111111111',zeroblob(32),'account','fixture client','fixture',4,1,1)`,
		`INSERT INTO credential_chats(credential_id,chat_id) VALUES('11111111111111111111111111111111','chat')`,
		`INSERT INTO send_permissions(credential_id,credential_revision,chat_id) VALUES('11111111111111111111111111111111',4,'chat')`,
		`INSERT INTO send_permission_modes VALUES('11111111111111111111111111111111',4,'selected',2)`,
		`INSERT INTO messages(id,account,chat_id,sender_id,source_chat,source_sender,source_message,from_me,state,source_kind,body_kind,local_revision,protocol_revision,original_sent_at,original_date_known,content_hash) VALUES('source','account','chat','sender','chat','sender','source',0,'active','original','voice',2,3,10,1,'` + strings.Repeat("a", 64) + `')`,
		`INSERT INTO extension_media_associations(id,account,message_id,local_revision,policy_generation,content_hash,ordinal,kind,protocol_reference,declared_bytes,availability,artifact_id,last_original_sha256) VALUES('media','account','source',2,9,'` + strings.Repeat("a", 64) + `',0,'voice',X'0102',4,'retained','artifact','` + strings.Repeat("b", 64) + `')`,
		`INSERT INTO extension_media_artifacts(id,reservation_id,media_id,account,message_id,local_revision,policy_generation,content_hash,budget,maximum_bytes,charged_bytes,lease_id,lease_until,state,sha256,bytes,actual_format,created_at,updated_at) VALUES('artifact','reservation','media','account','source',2,9,'` + strings.Repeat("a", 64) + `','originals',4,4,'lease',100,'retained','` + strings.Repeat("b", 64) + `',4,'ogg',1,1)`,
		`INSERT INTO extension_media_artifacts(id,reservation_id,media_id,account,message_id,local_revision,policy_generation,content_hash,budget,maximum_bytes,charged_bytes,lease_id,lease_until,state,input_artifact_id,created_at,updated_at) VALUES('decoded','decoded-reservation','media','account','source',2,9,'` + strings.Repeat("a", 64) + `','decoded_work',8,8,'decoded-lease',100,'reserved','artifact',1,1)`,
		`INSERT INTO extension_original_downloads(media_id,account,message_id,local_revision,policy_generation,content_hash,state,completed_sha256) VALUES('media','account','source',2,9,'` + strings.Repeat("a", 64) + `','complete','` + strings.Repeat("b", 64) + `')`,
		`INSERT INTO extension_extraction_versions VALUES('version','{}','` + strings.Repeat("c", 64) + `')`,
		`INSERT INTO extension_extraction_jobs(id,account,version_id,kind,media_id,artifact_id,original_sha256,message_id,local_revision,policy_generation,content_hash,work_hash,state,outcome,created_at,updated_at) VALUES('extraction','account','version','transcription','media','artifact','` + strings.Repeat("b", 64) + `','source',2,9,'` + strings.Repeat("a", 64) + `','` + strings.Repeat("d", 64) + `','complete','text',1,1)`,
		`INSERT INTO extension_roots(id,extraction_job_id,kind,text,text_sha256,text_bytes,media_id,provenance_json,source_count,created_at) VALUES('root','extraction','transcript','fixture voice text','` + strings.Repeat("e", 64) + `',18,'media','{}',1,1)`,
		`INSERT INTO extension_root_sources VALUES('root','account','source',2,9,'` + strings.Repeat("a", 64) + `',0,0)`,
		`INSERT INTO extension_extraction_outputs VALUES('extraction','root',0)`,
		`INSERT INTO extension_root_fts(root_id,text) VALUES('root','fixture voice text')`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func documentTableSnapshot(t *testing.T, q sqlQuerier, table string) string {
	t.Helper()
	rows, err := q.QueryContext(context.Background(), "SELECT * FROM "+table+" ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err = rows.Scan(destinations...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDocumentMigrationPreservesOriginalsTranscriptsReservationsAndClients(t *testing.T) {
	db := documentLegacyDB(t)
	tables := []string{"extension_media_artifacts", "extension_original_downloads", "extension_extraction_versions", "extension_extraction_jobs", "extension_roots", "extension_root_sources", "extension_extraction_outputs", "extension_root_fts", "credentials", "credential_chats", "send_permissions", "send_permission_modes", "messages", "archive_meta"}
	before := make(map[string]string)
	for _, table := range tables {
		before[table] = documentTableSnapshot(t, db, table)
	}
	var associationBefore string
	if err := db.QueryRow(`SELECT hex(protocol_reference)||'|'||last_original_sha256||'|'||artifact_id FROM extension_media_associations WHERE id='media'`).Scan(&associationBefore); err != nil {
		t.Fatal(err)
	}
	body, err := migrationFiles.ReadFile("migrations/014_documents.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		rows.Close()
		t.Fatal("document migration broke a foreign key")
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if err = finishMigrationForeignKeys(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		if got := documentTableSnapshot(t, db, table); got != before[table] {
			t.Fatalf("document migration changed existing %s", table)
		}
	}
	var associationAfter, filename string
	if err = db.QueryRow(`SELECT hex(protocol_reference)||'|'||last_original_sha256||'|'||artifact_id,filename FROM extension_media_associations WHERE id='media'`).Scan(&associationAfter, &filename); err != nil || associationBefore != associationAfter || filename != "" {
		t.Fatal("existing media association changed", err)
	}
	if _, err = db.Exec(`INSERT INTO extension_media_associations(id,account,message_id,local_revision,policy_generation,content_hash,ordinal,kind,protocol_reference,declared_bytes,availability,filename) VALUES('document','account','source',2,9,'` + strings.Repeat("a", 64) + `',1,'document',X'01',1,'not_retained','הצעה.pdf')`); err != nil {
		t.Fatal("document association unavailable", err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name IN ('extension_media_source','extension_media_artifact_links')`).Scan(&count); err != nil || count != 2 {
		t.Fatal("media indexes lost", err)
	}
	var foreignKeysEnabled int
	if err = db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeysEnabled); err != nil || foreignKeysEnabled != 1 {
		t.Fatal("migration disabled foreign-key enforcement", err)
	}
	if _, err = db.Exec(`INSERT INTO extension_media_associations(id,account,message_id,local_revision,policy_generation,content_hash,ordinal,kind,protocol_reference,declared_bytes,availability) VALUES('invalid-child','account','missing-source',1,9,'',0,'document',X'01',1,'unavailable')`); err == nil {
		t.Fatal("postmigration foreign-key violation was accepted")
	}
}

func TestDocumentMigrationRejectsActualForeignKeyViolationBeforeCounterReset(t *testing.T) {
	db := documentLegacyDB(t)
	before := documentTableSnapshot(t, db, "extension_media_artifacts")
	body, err := migrationFiles.ReadFile("migrations/014_documents.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE extension_media_artifacts SET media_id='missing-media' WHERE id='artifact'`); err != nil {
		t.Fatal("deferred fixture mutation unavailable", err)
	}
	if err = finishMigrationForeignKeys(context.Background(), tx); err != ErrArchiveUnavailable {
		t.Fatal("actual dangling child survived migration validation", err)
	}
	var deferred int
	if err = tx.QueryRow("PRAGMA defer_foreign_keys").Scan(&deferred); err != nil || deferred != 1 {
		t.Fatal("actual violations had their counters reset", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if after := documentTableSnapshot(t, db, "extension_media_artifacts"); after != before {
		t.Fatal("failed FK qualification changed existing ownership")
	}
}

func TestDocumentMigrationCancellationRollsBackWithoutLosingChildren(t *testing.T) {
	db := documentLegacyDB(t)
	before := documentTableSnapshot(t, db, "extension_media_artifacts")
	body, err := migrationFiles.ReadFile("migrations/014_documents.sql")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if err = finishMigrationForeignKeys(ctx, tx); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = tx.Commit(); !errors.Is(err, context.Canceled) && !errors.Is(err, sql.ErrTxDone) {
		t.Fatal("migration did not fail for transaction cancellation", err)
	}
	_ = tx.Rollback()
	if got := documentTableSnapshot(t, db, "extension_media_artifacts"); got != before {
		t.Fatal("cancelled migration changed existing original ownership")
	}
	var schema string
	if err = db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='extension_media_associations'`).Scan(&schema); err != nil || strings.Contains(schema, "filename") {
		t.Fatal("cancelled migration altered media schema", err)
	}
	var n int
	if err = db.QueryRow(`SELECT count(*) FROM extension_roots`).Scan(&n); err != nil || n != 1 {
		t.Fatal(fmt.Sprintf("cancelled migration lost transcript roots: %d", n), err)
	}
}

func TestDocumentMigrationMarksOnlyEligibleEmptyLegacyOriginals(t *testing.T) {
	for _, name := range []string{"eligible", "body", "caption", "edited", "view once", "barrier", "deleted", "expired", "unknown date", "association", "source tombstone", "chat tombstone", "unresolved deletion"} {
		t.Run(name, func(t *testing.T) {
			db := documentLegacyDB(t)
			legacy := event("legacy", "", 1, EventOriginal)
			legacy.BodyKind = MessageUnsupported
			if _, err := db.Exec(`INSERT INTO messages(id,account,chat_id,sender_id,source_chat,source_sender,source_message,from_me,state,source_kind,body_kind,local_revision,protocol_revision,original_sent_at,original_date_known,content_hash)
 VALUES('legacy','account','chat','sender','chat','sender','legacy',0,'active','original','unsupported',2,1,10,1,?)`, hashEvent(legacy)); err != nil {
				t.Fatal(err)
			}
			var mutation string
			switch name {
			case "body":
				mutation = `UPDATE messages SET body='existing original' WHERE id='legacy'`
			case "caption":
				mutation = `UPDATE messages SET caption='existing caption' WHERE id='legacy'`
			case "edited":
				mutation = `UPDATE messages SET edited=1 WHERE id='legacy'`
			case "view once":
				mutation = `UPDATE messages SET view_once=1 WHERE id='legacy'`
			case "barrier":
				mutation = `UPDATE messages SET revision_barrier='source_revision_conflict' WHERE id='legacy'`
			case "deleted":
				mutation = `UPDATE messages SET state='deleted' WHERE id='legacy'`
			case "expired":
				mutation = `UPDATE messages SET state='expired' WHERE id='legacy'`
			case "unknown date":
				mutation = `UPDATE messages SET original_date_known=0 WHERE id='legacy'`
			case "association":
				mutation = `INSERT INTO extension_media_associations(id,account,message_id,local_revision,policy_generation,content_hash,ordinal,kind,protocol_reference,declared_bytes,availability) SELECT 'legacy-media','account',id,local_revision,9,content_hash,0,'image',X'01',1,'not_retained' FROM messages WHERE id='legacy'`
			case "source tombstone":
				mutation = `INSERT INTO source_tombstones VALUES('account','chat','sender','legacy',0,1)`
			case "chat tombstone":
				mutation = `INSERT INTO chat_tombstones VALUES('account','chat',10)`
			case "unresolved deletion":
				mutation = `INSERT INTO unresolved_deletions VALUES('account','chat','legacy')`
			}
			if mutation != "" {
				if _, err := db.Exec(mutation); err != nil {
					t.Fatal(err)
				}
			}
			body, err := migrationFiles.ReadFile("migrations/014_documents.sql")
			if err != nil {
				t.Fatal(err)
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err = tx.Exec(string(body)); err != nil {
				t.Fatal(err)
			}
			if err = finishMigrationForeignKeys(context.Background(), tx); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			var markers int
			if err = db.QueryRow(`SELECT count(*) FROM extension_document_promotions WHERE message_id='legacy'`).Scan(&markers); err != nil {
				t.Fatal(err)
			}
			want := 0
			if name == "eligible" {
				want = 1
			}
			if markers != want {
				t.Fatalf("legacy %s promotion evidence count %d, want %d", name, markers, want)
			}
		})
	}
}
