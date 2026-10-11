package archive

// Error is a fixed body-free boundary code.
type Error string

func (e Error) Error() string { return string(e) }

const (
	ErrInstanceAlreadyRunning  Error = "instance_already_running"
	ErrInvalidPolicy           Error = "invalid_policy"
	ErrPolicyChanged           Error = "policy_changed"
	ErrInvalidEvent            Error = "invalid_event"
	ErrIdentityAliasConflict   Error = "identity_alias_conflict"
	ErrSourceRevisionConflict  Error = "source_revision_conflict"
	ErrMutationMetadataMissing Error = "mutation_metadata_missing"
	ErrQueueLimitExceeded      Error = "queue_limit_exceeded"
	ErrReconciliationRequired  Error = "reconciliation_required"
	ErrArchiveUnavailable      Error = "archive_unavailable"
	ErrSourceUnavailable       Error = "source_unavailable"
	ErrInvalidQuery            Error = "invalid_query"
	ErrCursorStale             Error = "cursor_stale"
	ErrUnauthorized            Error = "unauthorized"
	ErrUnsafeDataPath          Error = "unsafe_data_path"
	ErrFTS5Unavailable         Error = "fts5_unavailable"
	ErrSchemaChanged           Error = "schema_changed"
	ErrSchemaTooNew            Error = "schema_too_new"
)
