package archive

// Fixed body-free extension boundary errors.
const (
	ErrNoWork           Error = "no_work"
	ErrLimitExceeded    Error = "limit_exceeded"
	ErrInvalidExtension Error = "invalid_extension"
	ErrQuotaExceeded    Error = "waiting_for_disk_budget"
)
