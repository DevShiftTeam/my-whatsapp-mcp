package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

type rebuildProjection struct {
	table, columns string
	limit          int
}

// Fixed metadata manifest: no message body, derived content or jobs can enter
// these projections. Accounts precede all foreign keys; scopes keep opaque IDs.
var rebuildProjections = []rebuildProjection{
	{"accounts", "account", 1},
	{"identities", "id,account,source_identity,name", 1000000},
	{"identity_aliases", "account,alias,canonical,provenance", 1000000},
	{"chats", "id,account,source_chat,name,observed_oldest,observed_latest", 100000},
	{"chat_aliases", "account,alias,canonical,provenance", 100000},
	{"archive_policy", "id,generation,date_mode,import_start,chat_mode,rolling_retention", 1},
	{"policy_chats", "chat_id", 100000},
	{"credentials", "id,digest,account,label,principal,revision,all_chats,from_time,until_time,rolling,created_at,expires_at,revoked", 1024},
	{"credential_chats", "credential_id,chat_id", 1000000},
	{"send_permissions", "credential_id,credential_revision,chat_id", 1000000},
	{"send_permission_modes", "credential_id,credential_revision,mode,revision", 1024},
	{"send_requests", "credential_id,request_id,account,chat_id,digest,protocol_id,state,accepted_at", 1000000},
	{"source_tombstones", "account,chat,sender,message,from_me,protocol_revision", 1000000},
	{"chat_tombstones", "account,chat,cutoff", 100000},
	{"unresolved_deletions", "account,chat,message", 1000000},
	{"extension_original_budget", "id,maximum_bytes,revision", 1},
}

// BootstrapRebuild reads only a sealed coherent backup. It never opens the
// source as a Store or changes its safety authority. Destination creation is
// exclusive; an existing destination is never appended, reset or repaired.
func BootstrapRebuild(ctx context.Context, sourceArchiveDir, destinationArchiveDir string, o RebuildOptions) (receipt RebuildReceipt, err error) {
	if !validString(o.ExpectedAccount, 512) || !validString(o.DeviceID, 512) || o.Boundary.IsZero() || !validReadTime(o.Boundary) || len(o.ExpectedSourceSHA256) != 64 {
		return receipt, ErrInvalidEvent
	}
	if b, e := hex.DecodeString(o.ExpectedSourceSHA256); e != nil || len(b) != 32 {
		return receipt, ErrInvalidEvent
	}
	path, err := originalCacheExistingArchive(sourceArchiveDir, false)
	if err != nil {
		return receipt, err
	}
	if err = rebuildOwned(sourceArchiveDir, true); err != nil {
		return receipt, err
	}
	if err = rebuildOwned(path, false); err != nil {
		return receipt, err
	}
	before, err := rebuildFileHash(path)
	if err != nil || before != o.ExpectedSourceSHA256 {
		return receipt, ErrArchiveUnavailable
	}
	if _, e := os.Lstat(filepath.Join(sourceArchiveDir, credentialFenceMarker+".pending")); !errors.Is(e, os.ErrNotExist) {
		return receipt, ErrUnauthorized
	}
	marker := filepath.Join(sourceArchiveDir, credentialFenceMarker)
	if err = rebuildOwned(marker, false); err != nil {
		return receipt, err
	}
	if err = checkPrivateFile(marker); err != nil {
		return receipt, err
	}
	mf, err := os.Open(marker)
	if err != nil {
		return receipt, ErrUnauthorized
	}
	denied, readErr := io.ReadAll(io.LimitReader(mf, maxCredentialFenceBytes+1))
	closeErr := mf.Close()
	if readErr != nil || closeErr != nil || len(denied) > maxCredentialFenceBytes || !strings.HasPrefix(string(denied), "v1\n") || !strings.HasSuffix(string(denied), "\n") {
		return receipt, ErrUnauthorized
	}
	for _, id := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(string(denied), "v1\n"), "\n"), "\n") {
		if id != "" && id != "*" && !validCredentialID(CredentialID(id)) {
			return receipt, ErrUnauthorized
		}
	}
	if !filepath.IsAbs(destinationArchiveDir) || filepath.Clean(destinationArchiveDir) != destinationArchiveDir || destinationArchiveDir == sourceArchiveDir {
		return receipt, ErrUnsafeDataPath
	}
	if _, e := os.Lstat(destinationArchiveDir); e == nil {
		return readExistingRebuildReceipt(ctx, destinationArchiveDir, o)
	} else if !errors.Is(e, os.ErrNotExist) {
		return receipt, ErrUnsafeDataPath
	}
	// Validate parents without changing their modes or creating source-adjacent
	// paths implicitly. The profile owner supplies its private staging parent.
	parent := filepath.Dir(destinationArchiveDir)
	for part := parent; ; part = filepath.Dir(part) {
		st, e := os.Lstat(part)
		if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return receipt, ErrUnsafeDataPath
		}
		if filepath.Dir(part) == part {
			break
		}
	}
	if err = os.Mkdir(destinationArchiveDir, 0700); err != nil {
		return receipt, ErrUnsafeDataPath
	}
	// An interrupted import leaves an explicit incomplete marker. Normal Open
	// refuses it; an operator may discard this independently owned staging dir.
	incomplete := filepath.Join(destinationArchiveDir, "rebuild-incomplete.marker")
	if err = os.WriteFile(incomplete, []byte("v1\n"), 0600); err != nil {
		return receipt, ErrArchiveUnavailable
	}
	if err = os.WriteFile(filepath.Join(destinationArchiveDir, credentialFenceMarker), denied, 0600); err != nil {
		return receipt, ErrArchiveUnavailable
	}
	for _, name := range []string{incomplete, filepath.Join(destinationArchiveDir, credentialFenceMarker)} {
		f, e := os.OpenFile(name, os.O_RDWR, 0)
		if e != nil {
			return receipt, ErrArchiveUnavailable
		}
		e = f.Sync()
		ce := f.Close()
		if e != nil || ce != nil {
			return receipt, ErrArchiveUnavailable
		}
	}
	if err = syncDir(destinationArchiveDir); err != nil {
		return receipt, err
	}
	dest, err := Open(ctx, destinationArchiveDir, Options{rebuildBootstrap: true})
	if err != nil {
		return receipt, err
	}
	defer func() {
		if e := dest.Close(); err == nil && e != nil {
			err = e
		}
	}()
	base := url.URL{Scheme: "file", Path: path}
	base.RawQuery = url.Values{"mode": {"ro"}, "immutable": {"1"}, "_foreign_keys": {"on"}}.Encode()
	source, err := sql.Open(queryOnlyDriverName, base.String())
	if err != nil {
		return receipt, ErrArchiveUnavailable
	}
	defer source.Close()
	source.SetMaxOpenConns(1)
	src, err := source.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return receipt, ErrArchiveUnavailable
	}
	defer src.Rollback()
	sourceVersion, err := validateCompatibleSnapshotMigrations(ctx, src)
	if err != nil {
		return receipt, err
	}
	if sourceVersion >= 20 {
		var intents int
		if err = src.QueryRowContext(ctx, "SELECT count(*) FROM owner_voice_redownload_intents").Scan(&intents); err != nil {
			return receipt, err
		}
		// Reconstruction cannot silently discard request replay-denial receipts.
		if intents != 0 {
			return receipt, ErrSchemaChanged
		}
	}
	if sourceVersion >= 19 {
		var evidence int
		if err = src.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM history_uncertainty_coverage)+(SELECT count(*) FROM history_uncertainty_reasons)+(SELECT count(*) FROM sync_gaps WHERE code=?)", historyUncertaintyGapCode).Scan(&evidence); err != nil {
			return receipt, err
		}
		if evidence != 0 {
			return receipt, ErrSchemaChanged
		}
	}
	if sourceVersion >= 17 {
		accepted, _, err := acceptedPinnedHistoryGaps(ctx, src, o.ExpectedAccount)
		if err != nil {
			return receipt, err
		}
		if accepted != 0 {
			return receipt, ErrSchemaChanged
		}
	}
	if sourceVersion >= 18 {
		var evidence int
		if err = src.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM rebuild_history_selection)+(SELECT count(*) FROM rebuild_history_selection_audit)+(SELECT count(*) FROM rebuild_history_bodies)+(SELECT count(*) FROM rebuild_history_pages)+(SELECT count(*) FROM rebuild_history_frontiers)").Scan(&evidence); err != nil {
			return receipt, err
		}
		if evidence != 0 {
			return receipt, ErrSchemaChanged
		}
	}
	var account string
	var accounts int
	if err = src.QueryRowContext(ctx, "SELECT count(*),COALESCE(min(account),'') FROM accounts").Scan(&accounts, &account); err != nil || accounts != 1 || account != o.ExpectedAccount {
		return receipt, ErrUnauthorized
	}
	tx, err := dest.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return receipt, ErrArchiveUnavailable
	}
	defer tx.Rollback()
	variant, e := classifyRebuildSourceSchema(ctx, src, tx)
	if e != nil {
		return receipt, e
	}
	var check string
	if err = src.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&check); err != nil || check != "ok" {
		return receipt, ErrArchiveUnavailable
	}
	if err = rebuildCheckForeignKeys(ctx, src); err != nil {
		return receipt, err
	}
	generation, err := randomID()
	if err != nil {
		return receipt, err
	}
	receipt = RebuildReceipt{RebuildProvenance: RebuildProvenance{Generation: generation, Account: account, DeviceID: o.DeviceID, SourceSHA256: before, Boundary: fromMillis(unixms(o.Boundary))}, Tables: map[string]RebuildTableDigest{}}
	err = src.QueryRowContext(ctx, "SELECT reason,epoch FROM account_safety WHERE account=?", account).Scan(&receipt.OriginalFenceReason, &receipt.OriginalFenceEpoch)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return receipt, ErrArchiveUnavailable
	}
	if _, err = tx.ExecContext(ctx, rebuildSchema); err != nil {
		return receipt, ErrArchiveUnavailable
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM extension_original_budget"); err != nil {
		return receipt, ErrArchiveUnavailable
	}
	for _, projection := range rebuildProjections {
		digest, e := rebuildCopyProjection(ctx, src, tx, projection)
		if e != nil {
			return receipt, e
		}
		receipt.Tables[projection.table] = digest
	}
	if sourceVersion >= 17 {
		if err = rebuildCopyUnknownHistoryGaps(ctx, src, tx, account, receipt.Tables); err != nil {
			return receipt, err
		}
	}
	// This is a separate physical source namespace. Its local safety row starts
	// clean while the original fence remains sealed in provenance. Credential
	// checks require the row even before the first new source observation.
	if _, err = tx.ExecContext(ctx, "INSERT INTO account_safety(account) VALUES(?)", account); err != nil {
		return receipt, ErrArchiveUnavailable
	}
	if _, err = tx.ExecContext(ctx, "UPDATE chats SET observed_oldest=0,observed_latest=0"); err != nil {
		return receipt, ErrArchiveUnavailable
	}
	if variant != rebuildSourceOrdinary {
		parent, e := readForwardRebuildReceipt(ctx, src, receipt.RebuildProvenance)
		if e != nil {
			return receipt, e
		}
		receipt.Ancestors = append([]RebuildProvenance{parent.RebuildProvenance}, parent.Ancestors...)
		if err = copyForwardGuards(ctx, src, tx); err != nil {
			return receipt, fixedSQLError(err)
		}
		if err = copyForwardEvidence(ctx, src, tx, variant, parent, &receipt, o.Boundary); err != nil {
			return receipt, fixedSQLError(err)
		}
	}
	if err = rebuildCopyGuards(ctx, src, tx); err != nil {
		return receipt, err
	}
	if err = rebuildCopyAnchors(ctx, src, tx, unixms(o.Boundary)); err != nil {
		return receipt, err
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return receipt, ErrArchiveUnavailable
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO rebuild_provenance VALUES(1,?,?,?,?,?,?,?,?)", generation, account, o.DeviceID, before, unixms(o.Boundary), receipt.OriginalFenceReason, receipt.OriginalFenceEpoch, string(encoded)); err != nil {
		return receipt, ErrArchiveUnavailable
	}
	if err = initializeRebuildOperation(ctx, tx, receipt.RebuildProvenance, o.Boundary); err != nil {
		return receipt, fixedSQLError(err)
	}
	if err = rebuildCheckForeignKeys(ctx, tx); err != nil {
		return receipt, err
	}
	after, e := rebuildFileHash(path)
	if e != nil || after != before {
		return receipt, ErrArchiveUnavailable
	}
	if err = src.Commit(); err != nil {
		return receipt, ErrArchiveUnavailable
	}
	if err = tx.Commit(); err != nil {
		return receipt, ErrArchiveUnavailable
	}
	if _, err = dest.writeDB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return receipt, ErrArchiveUnavailable
	}
	if err = os.Remove(incomplete); err != nil {
		return receipt, ErrArchiveUnavailable
	}
	if err = syncDir(destinationArchiveDir); err != nil {
		return receipt, err
	}
	return receipt, nil
}

// A fresh physical namespace cannot erase accepted source uncertainty. These
// bounded projections carry only owner diagnostics and generic coverage, never
// old content or request authority. Prior schemas have no such tables.
func rebuildCopyUnknownHistoryGaps(ctx context.Context, src, dst *sql.Tx, account string, digests map[string]RebuildTableDigest) error {
	var epoch, total int64
	err := src.QueryRowContext(ctx, "SELECT epoch,total_observations FROM unknown_history_coverage WHERE account=?", account).Scan(&epoch, &total)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fixedSQLError(err)
	}
	var codes, warning int
	if queryErr := src.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM unknown_history_codes WHERE account=?),(SELECT count(*) FROM sync_gaps WHERE account=? AND chat='' AND code='unknown_history_notice')", account, account).Scan(&codes, &warning); queryErr != nil {
		return fixedSQLError(queryErr)
	}
	if errors.Is(err, sql.ErrNoRows) {
		if codes != 0 || warning != 0 {
			return ErrReconciliationRequired
		}
		return nil
	}
	if epoch <= 0 || total <= 0 || codes < 1 || codes > MaxUnknownHistoryGapCodes || warning != 1 {
		return ErrReconciliationRequired
	}
	rows, err := src.QueryContext(ctx, "SELECT raw_stub,count,first_seen,last_seen FROM unknown_history_codes WHERE account=? ORDER BY raw_stub LIMIT ?", account, MaxUnknownHistoryGapCodes+1)
	if err != nil {
		return fixedSQLError(err)
	}
	var sum int64
	for rows.Next() {
		var stub, count, first, last int64
		if err = rows.Scan(&stub, &count, &first, &last); err != nil || stub < 1 || stub > 2147483647 || count <= 0 || count > total-sum || last < first || !validReadTime(fromMillis(first)) || !validReadTime(fromMillis(last)) {
			rows.Close()
			return ErrReconciliationRequired
		}
		sum += count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fixedSQLError(err)
	}
	if sum != total {
		return ErrReconciliationRequired
	}
	for _, projection := range []rebuildProjection{{"unknown_history_coverage", "account,epoch,total_observations", 1}, {"unknown_history_codes", "account,raw_stub,count,first_seen,last_seen", MaxUnknownHistoryGapCodes}} {
		digest, err := rebuildCopyProjection(ctx, src, dst, projection)
		if err != nil {
			return err
		}
		digests[projection.table] = digest
	}
	if _, err = dst.ExecContext(ctx, "INSERT INTO sync_gaps(account,chat,code) VALUES(?,'','unknown_history_notice')", account); err != nil {
		return fixedSQLError(err)
	}
	_, err = dst.ExecContext(ctx, "INSERT INTO sync_state(account,chat,connection,coverage_unknown) VALUES(?,'','disconnected',1)", account)
	return fixedSQLError(err)
}

func rebuildFileHash(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", ErrUnsafeDataPath
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", ErrArchiveUnavailable
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// A repeated completed operation is a read-only receipt lookup. Any mismatch,
// active WAL or interrupted import fails; there is never an overwrite path.
func readExistingRebuildReceipt(ctx context.Context, dir string, o RebuildOptions) (RebuildReceipt, error) {
	var receipt RebuildReceipt
	if _, e := os.Lstat(filepath.Join(dir, "rebuild-incomplete.marker")); !errors.Is(e, os.ErrNotExist) {
		return receipt, ErrReconciliationRequired
	}
	path, e := originalCacheExistingArchive(dir, false)
	if e != nil {
		return receipt, e
	}
	if e = rebuildOwned(path, false); e != nil {
		return receipt, e
	}
	base := url.URL{Scheme: "file", Path: path}
	base.RawQuery = url.Values{"mode": {"ro"}, "immutable": {"1"}}.Encode()
	db, e := sql.Open(queryOnlyDriverName, base.String())
	if e != nil {
		return receipt, ErrArchiveUnavailable
	}
	defer db.Close()
	var encoded string
	if e = db.QueryRowContext(ctx, "SELECT receipt FROM rebuild_provenance WHERE id=1").Scan(&encoded); e != nil || len(encoded) > 65536 || json.Unmarshal([]byte(encoded), &receipt) != nil {
		return receipt, ErrUnsafeDataPath
	}
	if validateRebuildLineage(receipt) != nil {
		return receipt, ErrReconciliationRequired
	}
	if receipt.Account != o.ExpectedAccount || receipt.DeviceID != o.DeviceID || receipt.SourceSHA256 != o.ExpectedSourceSHA256 || unixms(receipt.Boundary) != unixms(o.Boundary) || len(receipt.Generation) != 32 {
		return RebuildReceipt{}, ErrUnsafeDataPath
	}
	return receipt, nil
}
func rebuildSchemaRows(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, e := tx.QueryContext(ctx, "SELECT type,name,tbl_name,COALESCE(sql,'') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name")
	if e != nil {
		return nil, ErrSchemaChanged
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ, name, table, statement string
		if e = rows.Scan(&typ, &name, &table, &statement); e != nil {
			return nil, ErrSchemaChanged
		}
		out = append(out, typ+"\x00"+name+"\x00"+table+"\x00"+statement)
	}
	return out, rows.Err()
}
func rebuildCheckForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, e := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if e != nil {
		return ErrArchiveUnavailable
	}
	defer rows.Close()
	if rows.Next() {
		return ErrArchiveUnavailable
	}
	return fixedSQLError(rows.Err())
}

func rebuildCopyProjection(ctx context.Context, src, dst *sql.Tx, p rebuildProjection) (RebuildTableDigest, error) {
	out := RebuildTableDigest{}
	columns := strings.Split(p.columns, ",")
	rows, e := src.QueryContext(ctx, "SELECT "+p.columns+" FROM "+p.table+" ORDER BY "+p.columns+" LIMIT ?", p.limit+1)
	if e != nil {
		return out, ErrSchemaChanged
	}
	defer rows.Close()
	types, e := rows.ColumnTypes()
	if e != nil {
		return out, ErrSchemaChanged
	}
	h := sha256.New()
	encoder := json.NewEncoder(h)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",")
	for rows.Next() {
		out.Rows++
		if out.Rows > int64(p.limit) {
			return out, ErrArchiveUnavailable
		}
		values := make([]any, len(columns))
		refs := make([]any, len(columns))
		for i := range values {
			refs[i] = &values[i]
		}
		if e = rows.Scan(refs...); e != nil {
			return out, ErrArchiveUnavailable
		}
		for i, value := range values {
			column := columns[i]
			switch v := value.(type) {
			case string:
				if types[i].DatabaseTypeName() != "TEXT" || len(v) > 4096 || !utf8.ValidString(v) || strings.IndexByte(v, 0) >= 0 {
					return out, ErrInvalidEvent
				}
				switch column {
				case "date_mode":
					if v != string(DateSince) && v != string(DateAllAvailable) {
						return out, ErrInvalidEvent
					}
				case "chat_mode":
					if v != string(ChatsAll) && v != string(ChatsSelective) {
						return out, ErrInvalidEvent
					}
				case "provenance":
					if v != string(AliasConnectorMapping) && v != string(AliasSessionMetadata) {
						return out, ErrInvalidEvent
					}
				case "account", "chat", "sender", "message", "source_chat", "source_identity", "alias", "canonical":
					if !validString(v, 512) {
						return out, ErrInvalidEvent
					}
				}
			case int64:
				gapTimestamp := p.table == "unknown_history_codes" && (column == "first_seen" || column == "last_seen")
				if types[i].DatabaseTypeName() != "INTEGER" || (v < 0 && !gapTimestamp) || (gapTimestamp && !validReadTime(fromMillis(v))) {
					return out, ErrInvalidEvent
				}
				switch column {
				case "from_me", "date_known", "all_chats", "revoked":
					if v > 1 {
						return out, ErrInvalidEvent
					}
				case "generation", "revision", "credential_revision":
					if v == 0 {
						return out, ErrInvalidEvent
					}
				}
			case []byte:
				if types[i].DatabaseTypeName() != "BLOB" || column != "digest" || len(v) != 32 {
					return out, ErrInvalidEvent
				}
			default:
				return out, ErrInvalidEvent
			}
		}
		if e = encoder.Encode(values); e != nil {
			return out, ErrArchiveUnavailable
		}
		if _, e = dst.ExecContext(ctx, "INSERT INTO "+p.table+"("+p.columns+") VALUES("+placeholders+")", values...); e != nil {
			return out, ErrArchiveUnavailable
		}
	}
	out.SHA256 = hex.EncodeToString(h.Sum(nil))
	return out, fixedSQLError(rows.Err())
}

func rebuildCopyGuards(ctx context.Context, src, dst *sql.Tx) error {
	if err := rebuildValidateScalarTypes(ctx, src, "messages", []string{"account", "source_chat", "source_sender", "source_message", "revision_barrier", "source_kind", "state"}, []string{"from_me", "original_sent_at", "original_date_known", "expires_at", "view_once", "protocol_revision"}); err != nil {
		return err
	}
	rows, e := src.QueryContext(ctx, "SELECT account,source_chat,source_sender,source_message,from_me,original_sent_at,original_date_known,expires_at,view_once,protocol_revision,revision_barrier,source_kind,state FROM messages LIMIT 1000001")
	if e != nil {
		return ErrSchemaChanged
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		if n > 1000000 {
			return ErrArchiveUnavailable
		}
		var k SourceKey
		var sent, expires, revision int64
		var known, view bool
		var barrier, kind, state string
		if e = rows.Scan(&k.Account, &k.Chat, &k.Sender, &k.Message, &k.FromMe, &sent, &known, &expires, &view, &revision, &barrier, &kind, &state); e != nil {
			return ErrInvalidEvent
		}
		if !validKey(k) || revision < 0 || expires < 0 || sent < 0 || len(barrier) > 128 {
			return ErrInvalidEvent
		}
		if known && sent == 0 {
			return ErrInvalidEvent
		}
		switch SourceEventKind(kind) {
		case EventOriginal, EventEditInvalidation, EventEditContent, EventDelete:
		default:
			return ErrInvalidEvent
		}
		switch state {
		case "active", "deleted", "expired", "retention_expired", "excluded", "invalidated":
		default:
			return ErrInvalidEvent
		}
		if barrier != "" && barrier != string(ErrSourceRevisionConflict) && barrier != string(ErrMutationMetadataMissing) {
			return ErrInvalidEvent
		}
		if e = mergeForwardGuard(ctx, dst, k, RebuildGuard{OriginalSentAt: fromMillis(sent), OriginalDateKnown: known, ExpiresAt: fromMillis(expires), ViewOnce: view, ProtocolRevision: revision, RevisionBarrier: barrier, SourceKind: SourceEventKind(kind), Deleted: state == "deleted" || state == "expired"}); e != nil {
			return e
		}
	}
	return fixedSQLError(rows.Err())
}
func rebuildCopyAnchors(ctx context.Context, src, dst *sql.Tx, now int64) error {
	if err := rebuildValidateScalarTypes(ctx, src, "source_anchors", []string{"account", "chat", "sender", "message"}, []string{"from_me", "original_sent_at", "date_known"}); err != nil {
		return err
	}
	rows, e := src.QueryContext(ctx, "SELECT account,chat,sender,message,from_me,original_sent_at,date_known FROM source_anchors UNION SELECT account,source_chat,source_sender,source_message,from_me,original_sent_at,original_date_known FROM messages LIMIT 1000001")
	if e != nil {
		return ErrSchemaChanged
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		if n > 1000000 {
			return ErrArchiveUnavailable
		}
		var a SourceAnchor
		var sent int64
		var known bool
		if e = rows.Scan(&a.Key.Account, &a.Key.Chat, &a.Key.Sender, &a.Key.Message, &a.Key.FromMe, &sent, &known); e != nil || !validKey(a.Key) {
			return ErrInvalidEvent
		}
		if !known || sent <= 0 {
			continue
		}
		k, err := canonicalKey(ctx, dst, a.Key)
		if err != nil {
			return ErrArchiveUnavailable
		}
		g, err := rebuildGuardQuery(ctx, dst, k)
		if err != nil {
			return err
		}
		if g.Deleted || g.ViewOnce || (!g.ExpiresAt.IsZero() && unixms(g.ExpiresAt) <= now) {
			continue
		}
		var cutoff int64
		err = dst.QueryRowContext(ctx, "SELECT cutoff FROM chat_tombstones WHERE account=? AND chat=?", k.Account, k.Chat).Scan(&cutoff)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ErrArchiveUnavailable
		}
		if err == nil && sent <= cutoff {
			continue
		}
		var existing int64
		err = dst.QueryRowContext(ctx, "SELECT original_sent_at FROM source_anchors WHERE account=? AND chat=? AND sender=? AND message=? AND from_me=?", k.Account, k.Chat, k.Sender, k.Message, k.FromMe).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ErrArchiveUnavailable
		}
		if err == nil && existing != sent {
			return ErrReconciliationRequired
		}
		if g.OriginalDateKnown && unixms(g.OriginalSentAt) != sent {
			return ErrReconciliationRequired
		}
		if _, e = dst.ExecContext(ctx, "INSERT OR IGNORE INTO source_anchors VALUES(?,?,?,?,?,?,1)", k.Account, k.Chat, k.Sender, k.Message, k.FromMe, sent); e != nil {
			return ErrArchiveUnavailable
		}
	}
	return fixedSQLError(rows.Err())
}

// Identifiers are compile-time reviewed projection constants, never input.
// SQLite affinity alone is insufficient: Go Scan otherwise coerces malformed
// scalar strings into numbers and silently loses the original representation.
func rebuildValidateScalarTypes(ctx context.Context, tx *sql.Tx, table string, textColumns, integerColumns []string) error {
	var invalid []string
	for _, column := range textColumns {
		invalid = append(invalid, "typeof("+column+")<>'text'")
	}
	for _, column := range integerColumns {
		invalid = append(invalid, "typeof("+column+")<>'integer'")
	}
	var found bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE "+strings.Join(invalid, " OR ")+")").Scan(&found); err != nil {
		return ErrArchiveUnavailable
	}
	if found {
		return ErrInvalidEvent
	}
	return nil
}
