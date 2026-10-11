package localui

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Bodokh/wa-indexer/internal/clientsetup"
)

const maxRemoteSecretResponse = 4096

func encodeRemoteSecret(value RemoteCredentialSecret) ([]byte, bool) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxRemoteSecretResponse {
		clear(data)
		return nil, false
	}
	return data, true
}

func (s *Server) revealRemote(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	app, ok := s.app.(RemoteCredentialApplication)
	if !ok {
		fail(w, 503, "client_connections_unavailable")
		return
	}
	var input ConnectionRevealRequest
	if !decode(w, r, &input) {
		return
	}
	if !validOpaque(input.ID) || input.Revision == 0 {
		fail(w, 400, "invalid_request")
		return
	}
	secret, err := app.RevealRemoteCredential(ctx, input)
	if err != nil {
		connectionError(w, err)
		return
	}
	publicURL, urlErr := clientsetup.NormalizeRemoteURL(secret.PublicURL)
	_, tokenOK := canonicalToken(secret.Token)
	if secret.ID != input.ID || secret.Revision != input.Revision || !tokenOK || urlErr != nil || publicURL != secret.PublicURL || !secret.ExpiresAt.After(time.Now()) {
		fail(w, 503, "client_connections_unavailable")
		return
	}
	data, ok := encodeRemoteSecret(secret)
	if !ok {
		fail(w, 503, "client_connections_unavailable")
		return
	}
	defer clear(data)
	// This explicit response is the only serialization of the transient secret.
	// No logging or returned diagnostic may format it, including encoding errors.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
