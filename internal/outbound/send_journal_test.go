//go:build sqlite_fts5

package outbound

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
)

type rejectedFinishStore struct{ Store }

func (rejectedFinishStore) FinishSend(context.Context, archive.CredentialGrant, string, string, time.Time) error {
	return archive.ErrArchiveUnavailable
}

type failedUploadProvider struct{ provider }

func (p *failedUploadProvider) Upload(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	p.uploaded++
	return whatsmeow.UploadResponse{}, errors.New("synthetic upload failure")
}

func TestFailedUploadRetainsUnknownReceiptIfJournalUpdateFails(t *testing.T) {
	s, grant, chat := sendFixture(t)
	store := rejectedFinishStore{s}
	client := &failedUploadProvider{}
	r := Request{ChatID: chat, RequestID: "failed-journal", Kind: "document", MIMEType: "application/pdf", Filename: "synthetic.pdf", DataBase64: base64.StdEncoding.EncodeToString([]byte("%PDF-1.7 synthetic"))}
	first, err := Send(context.Background(), store, client, grant, r)
	if err != nil || first.State != "outcome_unknown" {
		t.Fatal("uncommitted failure overclaimed a terminal receipt")
	}
	status, err := s.SendStatus(context.Background(), grant, r.RequestID)
	if err != nil || status.State != first.State {
		t.Fatal("returned receipt disagreed with durable send status")
	}
	repeated, err := Send(context.Background(), store, client, grant, r)
	if err != nil || repeated.State != "outcome_unknown" || !repeated.Duplicate || client.uploaded != 1 || client.sent != 0 {
		t.Fatal("uncertain failed journal permitted repeated provider work")
	}
}
