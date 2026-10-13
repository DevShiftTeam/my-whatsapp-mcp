package archive

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
)

type rebuildSourceVariant int

const (
	rebuildSourceOrdinary rebuildSourceVariant = iota
	rebuildSourceLegacy
	rebuildSourceContinuation
)

const rebuildRequestUpgradeDDL = "ALTER TABLE rebuild_requests ADD COLUMN operation_id TEXT NOT NULL DEFAULT ''; ALTER TABLE rebuild_requests ADD COLUMN operation_revision INTEGER NOT NULL DEFAULT 0;"

// Expected variants are compiled from trusted DDL in the empty destination,
// inside a rolled-back savepoint. The immutable source is never migrated.
func classifyRebuildSourceSchema(ctx context.Context, src, dst *sql.Tx) (rebuildSourceVariant, error) {
	version, err := validateCompatibleSnapshotMigrations(ctx, src)
	if err != nil {
		return 0, err
	}
	if version < 19 {
		if _, err = dst.ExecContext(ctx, "SAVEPOINT pre_uncertainty_schema"); err != nil {
			return 0, err
		}
		defer dst.ExecContext(ctx, "ROLLBACK TO pre_uncertainty_schema; RELEASE pre_uncertainty_schema")
		if _, err = dst.ExecContext(ctx, "DROP TABLE history_uncertainty_reasons; DROP TABLE history_uncertainty_coverage;"); err != nil {
			return 0, err
		}
	}
	if version < 18 {
		if _, err = dst.ExecContext(ctx, "SAVEPOINT pre_delivered_schema"); err != nil {
			return 0, err
		}
		defer dst.ExecContext(ctx, "ROLLBACK TO pre_delivered_schema; RELEASE pre_delivered_schema")
		if _, err = dst.ExecContext(ctx, "DROP TABLE rebuild_history_frontiers; DROP TABLE rebuild_history_pages; DROP TABLE rebuild_history_bodies; DROP TABLE rebuild_history_selection_audit; DROP TABLE rebuild_history_selection;"); err != nil {
			return 0, err
		}
	}
	if version < 17 {
		if _, err = dst.ExecContext(ctx, "SAVEPOINT pre_classified_schema"); err != nil {
			return 0, err
		}
		defer dst.ExecContext(ctx, "ROLLBACK TO pre_classified_schema; RELEASE pre_classified_schema")
		schemaFile := "migrations/011_snapshot_recovery.sql"
		if version == 15 {
			schemaFile = "migrations/015_classified_history_recovery.sql"
		} else if version == 16 {
			schemaFile = "migrations/016_classified_history_username.sql"
		}
		ddl, err := migrationFiles.ReadFile(schemaFile)
		if err != nil {
			return 0, ErrSchemaChanged
		}
		start := strings.Index(string(ddl), "CREATE TABLE snapshot_recovery_audit")
		if start < 0 {
			return 0, ErrSchemaChanged
		}
		auditDDL := string(ddl[start:])
		if version >= 15 {
			end := strings.Index(auditDDL, "INSERT INTO snapshot_recovery_audit(")
			if end < 0 {
				return 0, ErrSchemaChanged
			}
			auditDDL = auditDDL[:end]
		}
		if _, err = dst.ExecContext(ctx, "DROP TABLE unknown_history_codes; DROP TABLE unknown_history_coverage; DROP INDEX snapshot_recovery_one_prepared; DROP TABLE snapshot_recovery_audit;"+auditDDL); err != nil {
			return 0, err
		}
	}
	return classifyRebuildSourceSchemaVariant(ctx, src, dst)
}

func classifyRebuildSourceSchemaVariant(ctx context.Context, src, dst *sql.Tx) (rebuildSourceVariant, error) {
	actual, err := rebuildSchemaRows(ctx, src)
	if err != nil {
		return 0, err
	}
	expected, err := rebuildSchemaRows(ctx, dst)
	if err != nil {
		return 0, err
	}
	if reflect.DeepEqual(actual, expected) {
		return rebuildSourceOrdinary, nil
	}
	if _, err = dst.ExecContext(ctx, "SAVEPOINT forward_schema"); err != nil {
		return 0, err
	}
	defer dst.ExecContext(ctx, "ROLLBACK TO forward_schema; RELEASE forward_schema")
	if _, err = dst.ExecContext(ctx, rebuildSchema); err != nil {
		return 0, err
	}
	expected, err = rebuildSchemaRows(ctx, dst)
	if err != nil {
		return 0, err
	}
	if reflect.DeepEqual(actual, expected) {
		return rebuildSourceLegacy, nil
	}
	if _, err = dst.ExecContext(ctx, rebuildRequestUpgradeDDL+rebuildOperationSchema); err != nil {
		return 0, err
	}
	expected, err = rebuildSchemaRows(ctx, dst)
	if err != nil {
		return 0, err
	}
	if reflect.DeepEqual(actual, expected) {
		return rebuildSourceContinuation, nil
	}
	return 0, ErrSchemaChanged
}

func validRebuildProvenance(p RebuildProvenance) bool {
	generation, eg := hex.DecodeString(p.Generation)
	hash, eh := hex.DecodeString(p.SourceSHA256)
	return eg == nil && len(generation) == 16 && eh == nil && len(hash) == 32 && validString(p.Account, 512) && validString(p.DeviceID, 512) && !p.Boundary.IsZero() && validReadTime(p.Boundary)
}

func validateRebuildLineage(receipt RebuildReceipt) error {
	if !validRebuildProvenance(receipt.RebuildProvenance) || len(receipt.Ancestors)+1 > MaxForwardAncestorProfiles {
		return ErrReconciliationRequired
	}
	seen := map[string]bool{receipt.Generation: true}
	last := receipt.Boundary
	for _, p := range receipt.Ancestors {
		if !validRebuildProvenance(p) || p.Account != receipt.Account || p.DeviceID != receipt.DeviceID || seen[p.Generation] || p.Boundary.After(last) {
			return ErrReconciliationRequired
		}
		seen[p.Generation] = true
		last = p.Boundary
	}
	return nil
}

func readForwardRebuildReceipt(ctx context.Context, src *sql.Tx, next RebuildProvenance) (RebuildReceipt, error) {
	var receipt RebuildReceipt
	var p RebuildProvenance
	var boundary int64
	var encoded string
	err := src.QueryRowContext(ctx, "SELECT generation,account,device_id,source_sha256,boundary,fence_reason,fence_epoch,receipt FROM rebuild_provenance WHERE id=1").Scan(&p.Generation, &p.Account, &p.DeviceID, &p.SourceSHA256, &boundary, &p.OriginalFenceReason, &p.OriginalFenceEpoch, &encoded)
	p.Boundary = fromMillis(boundary)
	if err != nil || len(encoded) > 65536 || json.Unmarshal([]byte(encoded), &receipt) != nil || receipt.RebuildProvenance != p || validateRebuildLineage(receipt) != nil || p.Account != next.Account || p.DeviceID != next.DeviceID || p.Boundary.After(next.Boundary) || len(receipt.Ancestors)+1 >= MaxForwardAncestorProfiles {
		return receipt, ErrReconciliationRequired
	}
	return receipt, nil
}

// Every merge is canonical and monotone. Ambiguous dates or equal-revision
// metadata fail closed, rather than depending on SQL row order.
func mergeForwardGuard(ctx context.Context, dst *sql.Tx, key SourceKey, g RebuildGuard) error {
	k, err := canonicalKey(ctx, dst, key)
	if err != nil {
		return err
	}
	var old RebuildGuard
	var sent, expiry int64
	var kind string
	err = dst.QueryRowContext(ctx, "SELECT original_sent_at,date_known,expires_at,view_once,protocol_revision,revision_barrier,source_kind,deleted FROM rebuild_guards WHERE account=? AND chat=? AND sender=? AND message=? AND from_me=?", k.Account, k.Chat, k.Sender, k.Message, k.FromMe).Scan(&sent, &old.OriginalDateKnown, &expiry, &old.ViewOnce, &old.ProtocolRevision, &old.RevisionBarrier, &kind, &old.Deleted)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		old.OriginalSentAt = fromMillis(sent)
		old.ExpiresAt = fromMillis(expiry)
		old.SourceKind = SourceEventKind(kind)
		if old.OriginalDateKnown && g.OriginalDateKnown && !old.OriginalSentAt.Equal(g.OriginalSentAt) {
			return ErrReconciliationRequired
		}
		if old.ProtocolRevision == g.ProtocolRevision && ((old.SourceKind != "" && g.SourceKind != "" && old.SourceKind != g.SourceKind) || (old.RevisionBarrier != "" && g.RevisionBarrier != "" && old.RevisionBarrier != g.RevisionBarrier)) {
			return ErrReconciliationRequired
		}
		oldEdit := old.SourceKind == EventEditInvalidation || old.SourceKind == EventEditContent
		incomingEdit := g.SourceKind == EventEditInvalidation || g.SourceKind == EventEditContent
		// An original cannot supersede an edit restriction at any revision.
		// Check both orders because aliases may collapse previously distinct keys.
		if oldEdit && g.SourceKind == EventOriginal && g.ProtocolRevision > old.ProtocolRevision || incomingEdit && old.SourceKind == EventOriginal && old.ProtocolRevision > g.ProtocolRevision {
			return ErrReconciliationRequired
		}
		g.Deleted = g.Deleted || old.Deleted
		g.ViewOnce = g.ViewOnce || old.ViewOnce
		if !old.ExpiresAt.IsZero() && (g.ExpiresAt.IsZero() || old.ExpiresAt.Before(g.ExpiresAt)) {
			g.ExpiresAt = old.ExpiresAt
		}
		if old.OriginalDateKnown {
			g.OriginalDateKnown = true
			g.OriginalSentAt = old.OriginalSentAt
		}
		if old.ProtocolRevision > g.ProtocolRevision {
			g.ProtocolRevision = old.ProtocolRevision
			g.SourceKind = old.SourceKind
		}
		if g.SourceKind == "" {
			g.SourceKind = old.SourceKind
		}
		if old.RevisionBarrier != "" {
			g.RevisionBarrier = old.RevisionBarrier
		}
	}
	_, err = dst.ExecContext(ctx, "INSERT INTO rebuild_guards VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(account,chat,sender,message,from_me) DO UPDATE SET original_sent_at=excluded.original_sent_at,date_known=excluded.date_known,expires_at=excluded.expires_at,view_once=excluded.view_once,protocol_revision=excluded.protocol_revision,revision_barrier=excluded.revision_barrier,source_kind=excluded.source_kind,deleted=excluded.deleted", k.Account, k.Chat, k.Sender, k.Message, k.FromMe, unixms(g.OriginalSentAt), g.OriginalDateKnown, unixms(g.ExpiresAt), g.ViewOnce, g.ProtocolRevision, g.RevisionBarrier, g.SourceKind, g.Deleted)
	return err
}

func copyForwardGuards(ctx context.Context, src, dst *sql.Tx) error {
	if err := rebuildValidateScalarTypes(ctx, src, "rebuild_guards", []string{"account", "chat", "sender", "message", "revision_barrier", "source_kind"}, []string{"from_me", "original_sent_at", "date_known", "expires_at", "view_once", "protocol_revision", "deleted"}); err != nil {
		return err
	}
	rows, err := src.QueryContext(ctx, "SELECT account,chat,sender,message,from_me,original_sent_at,date_known,expires_at,view_once,protocol_revision,revision_barrier,source_kind,deleted FROM rebuild_guards LIMIT 1000001")
	if err != nil {
		return err
	}
	defer rows.Close()
	var count int
	for rows.Next() {
		count++
		if count > int(MaxRebuildMetadata) {
			return ErrLimitExceeded
		}
		var k SourceKey
		var g RebuildGuard
		var sent, expiry int64
		var kind string
		if err = rows.Scan(&k.Account, &k.Chat, &k.Sender, &k.Message, &k.FromMe, &sent, &g.OriginalDateKnown, &expiry, &g.ViewOnce, &g.ProtocolRevision, &g.RevisionBarrier, &kind, &g.Deleted); err != nil {
			return ErrInvalidEvent
		}
		if !validKey(k) || sent < 0 || expiry < 0 || g.ProtocolRevision < 0 || (g.OriginalDateKnown && sent == 0) {
			return ErrInvalidEvent
		}
		switch SourceEventKind(kind) {
		case "", EventOriginal, EventEditInvalidation, EventEditContent, EventDelete:
		default:
			return ErrInvalidEvent
		}
		if g.RevisionBarrier != "" && g.RevisionBarrier != string(ErrSourceRevisionConflict) && g.RevisionBarrier != string(ErrMutationMetadataMissing) {
			return ErrInvalidEvent
		}
		g.OriginalSentAt = fromMillis(sent)
		g.ExpiresAt = fromMillis(expiry)
		g.SourceKind = SourceEventKind(kind)
		if err = mergeForwardGuard(ctx, dst, k, g); err != nil {
			return err
		}
	}
	return rows.Err()
}

func copyForwardEvidence(ctx context.Context, src, dst *sql.Tx, variant rebuildSourceVariant, parent RebuildReceipt, next *RebuildReceipt, now time.Time) error {
	if _, err := dst.ExecContext(ctx, rebuildRequestUpgradeDDL+rebuildOperationSchema); err != nil {
		return err
	}
	requests := "id,generation,account,chat,sender,message,from_me,policy_generation,created_at,state,consumed_at"
	if variant == rebuildSourceContinuation {
		requests += ",operation_id,operation_revision"
	}
	projections := []rebuildProjection{{"rebuild_requests", requests, int(maxRebuildCopyLimit)}}
	if variant == rebuildSourceContinuation {
		projections = append(projections, rebuildProjection{"rebuild_operation", "id,operation_id,generation,policy_generation,target_start,revision,state,copy_limit,copy_spent,discovery_limit,discovery_spent,last_attempt_at,last_copy_chat,last_discovery_chat,queue_full,created_at,updated_at", 1}, rebuildProjection{"rebuild_commands", "command_id,expected_revision,action,result_revision", 1000000}, rebuildProjection{"rebuild_discovery_attempts", "id,operation_id,generation,account,chat,sender,message,from_me,original_sent_at,policy_generation,operation_revision,created_at", int(maxRebuildDiscoveryLimit)}, rebuildProjection{"rebuild_candidate_deferrals", "account,chat,sender,message,from_me,until_time", int(MaxRebuildMetadata)})
	}
	for _, p := range projections {
		digest, err := rebuildCopyProjection(ctx, src, dst, p)
		if err != nil {
			return err
		}
		next.Tables[p.table] = digest
	}
	generations := map[string]bool{parent.Generation: true}
	for _, p := range parent.Ancestors {
		generations[p.Generation] = true
	}
	for _, table := range []string{"rebuild_requests", "rebuild_discovery_attempts"} {
		rows, err := dst.QueryContext(ctx, "SELECT DISTINCT generation,account FROM "+table)
		if err != nil {
			return err
		}
		for rows.Next() {
			var generation, account string
			if err = rows.Scan(&generation, &account); err != nil || !generations[generation] || account != next.Account {
				rows.Close()
				return ErrReconciliationRequired
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	if variant == rebuildSourceLegacy {
		if _, err := dst.ExecContext(ctx, "UPDATE rebuild_requests SET operation_id=?,operation_revision=1", parent.Generation); err != nil {
			return err
		}
		if err := initializeRebuildOperation(ctx, dst, parent.RebuildProvenance, now); err != nil {
			return err
		}
	}
	o, err := loadRebuildOperation(ctx, dst)
	if err != nil {
		return err
	}
	var copies, discoveries int64
	if err = dst.QueryRowContext(ctx, "SELECT count(*) FROM rebuild_requests").Scan(&copies); err != nil {
		return err
	}
	if err = dst.QueryRowContext(ctx, "SELECT count(*) FROM rebuild_discovery_attempts").Scan(&discoveries); err != nil {
		return err
	}
	if o.Generation != parent.Generation || o.Revision == 0 || o.Revision >= uint64(1<<63-1) || o.CopySpent != copies || o.DiscoverySpent != discoveries || o.CopyLimit < copies || o.CopyLimit > maxRebuildCopyLimit || o.DiscoveryLimit < discoveries || o.DiscoveryLimit > maxRebuildDiscoveryLimit {
		return ErrReconciliationRequired
	}
	var policy Generation
	var target int64
	if err = dst.QueryRowContext(ctx, "SELECT generation,CASE WHEN date_mode='since' THEN import_start ELSE 0 END FROM archive_policy WHERE id=1").Scan(&policy, &target); err != nil {
		return err
	}
	if o.PolicyGeneration != policy || unixms(o.TargetStart) != target || !validString(o.OperationID, 128) {
		return ErrReconciliationRequired
	}
	var bad bool
	if err = dst.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM rebuild_requests WHERE operation_id<>? OR operation_revision<1 OR operation_revision>? OR policy_generation<1 OR created_at<=0 OR (state='consumed' AND consumed_at<=0)) OR EXISTS(SELECT 1 FROM rebuild_discovery_attempts WHERE operation_id<>? OR operation_revision<1 OR operation_revision>? OR original_sent_at<=0 OR policy_generation<1 OR created_at<=0)", o.OperationID, o.Revision, o.OperationID, o.Revision).Scan(&bad); err != nil {
		return err
	}
	if bad {
		return ErrReconciliationRequired
	}
	if err = dst.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM rebuild_commands WHERE length(command_id)=0 OR length(command_id)>128 OR expected_revision<1 OR result_revision<>expected_revision+1 OR result_revision>? OR action NOT IN('pause','resume','extend','cancel')) OR EXISTS(SELECT 1 FROM rebuild_candidate_deferrals WHERE account<>? OR from_me NOT IN(0,1)) OR EXISTS(SELECT 1 FROM rebuild_requests WHERE length(id)=0 OR length(id)>128 OR (state='pending' AND consumed_at<>0))", o.Revision, next.Account).Scan(&bad); err != nil {
		return err
	}
	if bad {
		return ErrReconciliationRequired
	}
	_, err = dst.ExecContext(ctx, "UPDATE rebuild_operation SET generation=?,revision=revision+1,updated_at=? WHERE id=1", next.Generation, unixms(now))
	return err
}
