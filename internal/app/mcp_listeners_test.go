package app

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPListenersNeverResolveNamesOrBindExternally(t *testing.T) {
	for _, address := range []string{"localhost:0", "0.0.0.0:0", "[::]:0", "127.0.0.1:http", "127.0.0.1:65536", "127.0.0.1:00", "[::1%zone]:0"} {
		if literalMCPLoopback(address) {
			t.Fatal("unsafe/noncanonical listener accepted")
		}
		if listener, err := newMCPLoopback(context.Background(), address); err == nil {
			listener.Close()
			t.Fatal("unsafe listener bound")
		}
	}
	for _, address := range []string{"127.0.0.1:0", "[::1]:0"} {
		if !literalMCPLoopback(address) {
			t.Fatal("literal loopback refused")
		}
	}
}
func TestPendingNativeTransportDoesNotConsumeCredentials(t *testing.T) {
	request := httptest.NewRequest("POST", "http://127.0.0.1/mcp", strings.NewReader("private-body-SENTINEL"))
	request.Header.Set("Authorization", "Bearer private-token-SENTINEL")
	response := httptest.NewRecorder()
	nativeTransportPending(response, request)
	if response.Code != 503 || strings.Contains(response.Body.String(), "SENTINEL") {
		t.Fatal("pending transport exposed request authority")
	}
}
