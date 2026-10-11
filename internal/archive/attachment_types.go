package archive

import "time"

// AttachmentSearchRequest narrows the readable archive. FileType is a kind,
// "pdf", or an exact MIME type; query and filename never contain operators.
type AttachmentSearchRequest struct {
	Query       string
	ChatIDs     []ChatID
	FileType    string
	Filename    string
	From, Until time.Time
	Before      int
	After       int
	Limit       int
	Cursor      string
}

type AttachmentView struct {
	AttachmentID          MediaID
	MessageID             MessageID
	ChatID                ChatID
	ChatName              string
	SenderID, SenderName  string
	SentAt                time.Time
	Kind                  MessageKind
	Filename, MIMEType    string
	DeclaredBytes, Bytes  int64
	Availability, Caption string
	Before, After         []MessageView
	Score                 float64
	Snapshot              ReadSnapshot
	original              *MediaArtifact
}

// OriginalArtifact is an internal Go capability. The artifact and its source
// are deliberately absent from the public JSON attachment metadata.
func (v AttachmentView) OriginalArtifact() (MediaArtifact, bool) {
	if v.original == nil || v.Availability != "retained" {
		return MediaArtifact{}, false
	}
	return *v.original, true
}

type AttachmentPage struct {
	Attachments []AttachmentView
	NextCursor  string
	Snapshot    ReadSnapshot
}

// This archive-issued witness covers association metadata and retained byte
// identity that can change without altering the message's source revision.
type attachmentResponsePin struct {
	ID           MediaID
	Source       SourceRef
	MetadataHash [32]byte
}
