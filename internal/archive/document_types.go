package archive

import (
	"strings"
	"time"
	"unicode/utf8"
)

const MaximumDocumentBytes int64 = 8 << 20

func validDocumentFilename(v string) bool {
	return len(v) <= 255 && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}

// DocumentRedeliverySource carries private source metadata for an explicit,
// bounded owner request. It grants neither a download nor read authority.
type DocumentRedeliverySource struct {
	Key            SourceKey
	Source         SourceRef
	OriginalSentAt time.Time
}

type DocumentDownloadRetry struct{ Queued, Skipped int }
