package remoteaccess

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPinOrigin(t *testing.T) {
	for _, origin := range []string{"https://saved.ngrok-free.app", "https://node.tail123.ts.net", "https://remote.example.com"} {
		if got, err := PinOrigin("", origin); err != nil || got != origin {
			t.Fatalf("canonical rejected: %q %v", got, err)
		}
	}
	for _, origin := range []string{"http://example.com", "https://example.com:443", "https://EXAMPLE.com", "https://example.com/", "https://example.com/mcp", "https://example.com?x", "https://example.com#x", "https://user@example.com", "https://127.0.0.1", "https://[::1]", "https://2130706433", "https://0177.0.0.1", "https://0x7f000001", "https://127.1", "https://localhost", "https://foo.local", "https://foo.internal", "https://foo.localhost", "https://foo.invalid", "https://example.com.", " https://example.com", "https://a_b.example", "https://-foo.example"} {
		if _, err := PinOrigin("", origin); !errors.Is(err, ErrInvalidOrigin) {
			t.Errorf("accepted %q", origin)
		}
	}
	if _, err := PinOrigin("https://saved.ngrok-free.app", "https://replacement.ngrok-free.app"); !errors.Is(err, ErrIdentityChanged) {
		t.Fatal("identity changed")
	}
	if _, err := PinOrigin("https://saved.ngrok-free.app", "https://saved.ngrok-free.app"); err != nil {
		t.Fatal(err)
	}
}
func TestIntentAndSecret(t *testing.T) {
	for _, provider := range []Provider{TailscalePrivate, TailscaleFunnel, Ngrok, Cloudflare} {
		if err := validateIntent(Intent{Version: 1, Provider: provider, SavedOrigin: "https://saved.ngrok-free.app"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, intent := range []Intent{{Version: 2, Provider: Ngrok}, {Version: 1, Provider: "NGROK"}, {Version: 1, Provider: Ngrok, NodeName: "../escape"}, {Version: 1, Provider: Ngrok, SavedOrigin: "http://bad.example"}} {
		if validateIntent(intent) == nil {
			t.Fatal("invalid intent accepted")
		}
	}
	secret := Secret("do-not-disclose")
	for _, printed := range []string{fmt.Sprint(secret), fmt.Sprintf("%#v", secret), fmt.Sprintf("%s", secret), fmt.Sprintf("%x", secret)} {
		if strings.Contains(printed, "do-not-disclose") || strings.Contains(printed, "646f2d6e6f74") {
			t.Fatal("secret printed")
		}
	}
	encoded, err := json.Marshal(struct{ Secret Secret }{secret})
	if err != nil || strings.Contains(string(encoded), "ZG8t") || strings.Contains(string(encoded), "do-not") {
		t.Fatal("secret JSON disclosed")
	}
}
