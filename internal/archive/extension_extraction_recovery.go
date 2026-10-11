package archive

import (
	"context"
	"database/sql"
)

type extensionExtractionRecoveryCommand struct {
	request ExtractionRecoveryRequest
}

func (c extensionExtractionRecoveryCommand) priority() bool  { return false }
func (c extensionExtractionRecoveryCommand) account() string { return c.request.Account }
func (c extensionExtractionRecoveryCommand) sensitivity() extensionSensitivity {
	return extensionBackground
}
func (c extensionExtractionRecoveryCommand) run(s *Store, ctx context.Context) (any, error) {
	var page ExtractionRecoveryPage
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		// Ownership inventory deliberately has no availability, state, expiry,
		// or artifact joins. Missing leases are corruption, never recovered work.
		rows, err := tx.QueryContext(ctx, `SELECT
		 CASE WHEN typeof(account)='text' AND length(CAST(account AS BLOB)) BETWEEN 1 AND 512 THEN account ELSE '' END,
		 CASE WHEN typeof(id)='text' AND length(CAST(id AS BLOB)) BETWEEN 1 AND 128 THEN id ELSE '' END,
		 CASE WHEN typeof(lease_id)='text' AND length(CAST(lease_id AS BLOB)) BETWEEN 1 AND 256 THEN lease_id ELSE '' END,
		 CASE WHEN typeof(media_id)='text' AND length(CAST(media_id AS BLOB)) BETWEEN 1 AND 128 THEN media_id ELSE '' END,
		 CASE WHEN typeof(artifact_id)='text' AND length(CAST(artifact_id AS BLOB)) BETWEEN 1 AND 128 THEN artifact_id ELSE '' END
		 FROM extension_extraction_jobs WHERE (account=? OR account=CAST(? AS BLOB)) AND input_pinned=1 AND (id>? OR id IS NULL OR id='') ORDER BY id LIMIT ?`, c.request.Account, c.request.Account, c.request.After, c.request.Limit+1)
		if err != nil {
			return fixedSQLError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var item PinnedExtraction
			if err = rows.Scan(&item.Account, &item.Lease.JobID, &item.Lease.LeaseID, &item.MediaID, &item.ArtifactID); err != nil {
				return fixedSQLError(err)
			}
			if item.Account != c.request.Account || !validString(item.Account, 512) || !validReadID(string(item.Lease.JobID)) || !extensionValidLease(item.Lease) || !validReadID(string(item.MediaID)) || !validReadID(string(item.ArtifactID)) {
				return ErrArchiveUnavailable
			}
			page.Items = append(page.Items, item)
		}
		if err = rows.Err(); err != nil {
			return fixedSQLError(err)
		}
		if len(page.Items) > c.request.Limit {
			page.Items = page.Items[:c.request.Limit]
			page.NextAfter = page.Items[len(page.Items)-1].Lease.JobID
		}
		return nil
	})
	if err != nil {
		return ExtractionRecoveryPage{}, err
	}
	return page, nil
}

func (s *Store) ListPinnedExtractions(ctx context.Context, request ExtractionRecoveryRequest) (ExtractionRecoveryPage, error) {
	if !validString(request.Account, 512) || (request.After != "" && !validReadID(string(request.After))) || request.Limit < 0 {
		return ExtractionRecoveryPage{}, ErrInvalidExtension
	}
	if request.Limit > 64 {
		return ExtractionRecoveryPage{}, ErrLimitExceeded
	}
	if request.Limit == 0 {
		request.Limit = 32
	}
	value, err := s.submitExtension(ctx, extensionExtractionRecoveryCommand{request: request}, int64(len(request.Account)+len(request.After)+64))
	if err != nil {
		return ExtractionRecoveryPage{}, err
	}
	return value.(ExtractionRecoveryPage), nil
}

var _ ExtractionRecoveryStore = (*Store)(nil)
