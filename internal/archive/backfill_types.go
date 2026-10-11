package archive

import "context"

// BackfillSelection is owner-private connector metadata, with authorization
// applied before per-chat ranking and the bounded result limit.
type BackfillSelection struct {
	Account          string
	PolicyGeneration Generation
	AllSelected      bool
	Chats            []ChatID
	Limit            int
}
type SelectedBackfillStore interface {
	SelectedBackfillAnchors(context.Context, BackfillSelection) ([]SourceAnchor, error)
}
