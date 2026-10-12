package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
)

const helperInstance = "00112233445566778899aabbccddeeff"

var helperKey = bytes.Repeat([]byte{42}, 32)

func helperMAC(fields ...string) string {
	mac := hmac.New(sha256.New, helperKey)
	for _, field := range fields {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		mac.Write(size[:])
		mac.Write([]byte(field))
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func helperPrivateRoot(t *testing.T, origin string) (string, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths, err := appdata.Prepare(filepath.Join(base, "helper-data"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	private := origin + "/#attach=" + helperInstance + "." + base64.RawURLEncoding.EncodeToString(helperKey)
	if err := appdata.WriteOwnerURL(paths, lease, private); err != nil {
		t.Fatal(err)
	}
	return paths.Root, private
}

func TestOwnerLinkVerifiesSignedFreshRepliesAndKeepsSecretsPrivate(t *testing.T) {
	const origin = "http://127.0.0.1:9000"
	for _, mode := range []string{"fresh", "expired", "replay", "wrong-instance", "wrong-demo", "wrong-proof", "redirect", "fake-health"} {
		t.Run(mode, func(t *testing.T) {
			var previous []byte
			var issued string
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, "http://192.0.2.1:1234/", 307)
					return
				}
				if mode == "fake-health" {
					w.Write([]byte("ready\n"))
					return
				}
				if r.Host != "127.0.0.1:9000" || r.Header.Get("Origin") != origin || r.URL.Path != "/api/owner/attach" {
					t.Error("helper changed approved Host/Origin")
					w.WriteHeader(400)
					return
				}
				var request struct {
					Instance, Nonce, Proof string
					Demo                   bool
					Expires                string `json:"expires_unix"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					w.WriteHeader(400)
					return
				}
				if request.Proof != helperMAC("wa-indexer-owner-attach/request/v1", origin, helperInstance, request.Nonce, "1", request.Expires) {
					t.Error("request proof not bound to approved authority/demo")
					w.WriteHeader(401)
					return
				}
				digest := sha256.Sum256([]byte(request.Nonce))
				token := base64.RawURLEncoding.EncodeToString(digest[:])
				expiry := strconv.FormatInt(time.Now().Unix()+30, 10)
				if mode == "expired" {
					expiry = strconv.FormatInt(time.Now().Unix()-1, 10)
				}
				reply := map[string]any{"instance": helperInstance, "nonce": request.Nonce, "demo": true, "bootstrap_token": token, "expires_unix": expiry,
					"proof": helperMAC("wa-indexer-owner-attach/response/v1", origin, helperInstance, request.Nonce, "1", request.Expires, token, expiry)}
				if mode == "wrong-instance" {
					reply["instance"] = strings.Repeat("0", 32)
				}
				if mode == "wrong-demo" {
					reply["demo"] = false
				}
				if mode == "wrong-proof" {
					reply["proof"] = base64.RawURLEncoding.EncodeToString(helperKey)
				}
				data, _ := json.Marshal(reply)
				if mode == "replay" && previous != nil {
					data = previous
				}
				previous = data
				issued = origin + "/#bootstrap=" + token + "\n"
				w.Header().Set("Content-Type", "application/json")
				w.Write(data)
			}))
			defer peer.Close()
			root, private := helperPrivateRoot(t, origin)
			args := []string{"owner-link", "--data-dir", root, "--demo", "--connect", strings.TrimPrefix(peer.URL, "http://")}
			var first string
			attempts := 1
			if mode == "fresh" || mode == "replay" {
				attempts = 2
			}
			for attempt := 0; attempt < attempts; attempt++ {
				var out, errs bytes.Buffer
				code := run(context.Background(), args, &out, &errs)
				success := mode == "fresh" || (mode == "replay" && attempt == 0)
				if success {
					if code != 0 || out.String() != issued || errs.Len() != 0 {
						t.Fatal("explicit helper did not print only a verified bootstrap")
					}
					if attempt > 0 && out.String() == first {
						t.Fatal("helper reused a bootstrap")
					}
					first = out.String()
				} else if code != 1 || out.Len() != 0 || errs.Len() == 0 {
					t.Fatal("unverified response supplied a login capability")
				}
				for _, secret := range []string{private, base64.RawURLEncoding.EncodeToString(helperKey), "#attach=", root} {
					if strings.Contains(errs.String(), secret) || strings.Contains(out.String(), secret) {
						t.Fatal("helper leaked private material")
					}
				}
			}
		})
	}
}

func TestOwnerHelperOptionsErrorsDoNotEchoMaliciousValuesOrCreateData(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing-SENTINEL")
	for _, command := range []string{"owner-link", "health"} {
		for _, flags := range [][]string{{"--connect", "secret-SENTINEL"}, {"--connect", "localhost:8123"}, {"--connect", "127.0.0.1:0"}, {"--connect", "127.0.0.1:8123", "--connect", "127.0.0.1:9000"}, {"--demo=secret-SENTINEL"}, {"--originals-budget", "64MiB"}, {"--listen", "127.0.0.1:8123"}} {
			args := append([]string{command, "--data-dir", root}, flags...)
			var out, errs bytes.Buffer
			if run(context.Background(), args, &out, &errs) != 2 || out.Len() != 0 || strings.Contains(errs.String(), "SENTINEL") || strings.Contains(errs.String(), root) {
				t.Fatal("helper flags accepted or disclosed supplied values")
			}
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("invalid helper flags created storage")
	}
}

func TestOwnerLinkRepeatedRequestsAfterOwnerProcessRestart(t *testing.T) {
	const origin = "http://127.0.0.1:9000"
	var activeInstance = helperInstance
	var activeKey = helperKey
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Instance, Nonce, Proof string
			Demo                   bool
			Expires                string `json:"expires_unix"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Instance != activeInstance {
			w.WriteHeader(401)
			return
		}
		digest := sha256.Sum256([]byte(request.Nonce))
		token := base64.RawURLEncoding.EncodeToString(digest[:])
		expiry := strconv.FormatInt(time.Now().Unix()+30, 10)
		mac := hmac.New(sha256.New, activeKey)
		for _, field := range []string{"wa-indexer-owner-attach/response/v1", origin, activeInstance, request.Nonce, "0", request.Expires, token, expiry} {
			var size [4]byte
			binary.BigEndian.PutUint32(size[:], uint32(len(field)))
			mac.Write(size[:])
			mac.Write([]byte(field))
		}
		reply := map[string]any{"instance": activeInstance, "nonce": request.Nonce, "demo": false, "bootstrap_token": token, "expires_unix": expiry, "proof": base64.RawURLEncoding.EncodeToString(mac.Sum(nil))}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reply)
	}))
	defer peer.Close()
	root, _ := helperPrivateRoot(t, origin)
	args := []string{"owner-link", "--data-dir", root, "--connect", strings.TrimPrefix(peer.URL, "http://")}
	var first, errs bytes.Buffer
	if run(context.Background(), args, &first, &errs) != 0 {
		t.Fatal("initial owner request failed")
	}
	// Stop the old process listener, then replace it with a fresh signed instance.
	actual := peer.Listener.Addr().String()
	peer.Close()
	activeInstance = strings.Repeat("1", 32)
	activeKey = bytes.Repeat([]byte{7}, 32)
	replacement := httptest.NewUnstartedServer(peer.Config.Handler)
	listener, err := net.Listen("tcp4", actual)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Listener.Close()
	replacement.Listener = listener
	replacement.Start()
	defer replacement.Close()
	var stale bytes.Buffer
	if run(context.Background(), args, &stale, &errs) != 1 || stale.Len() != 0 {
		t.Fatal("stale previous-instance file minted a login")
	}
	private := origin + "/#attach=" + activeInstance + "." + base64.RawURLEncoding.EncodeToString(activeKey)
	if err := os.WriteFile(filepath.Join(root, "owner-url"), []byte(private+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var second, third bytes.Buffer
	if run(context.Background(), args, &second, &errs) != 0 || run(context.Background(), args, &third, &errs) != 0 || second.String() == third.String() || second.String() == first.String() {
		t.Fatal("restarted owner did not return independent fresh bootstraps")
	}
	if strings.Contains(errs.String(), private) {
		t.Fatal("restart failure leaked durable owner capability")
	}
}
