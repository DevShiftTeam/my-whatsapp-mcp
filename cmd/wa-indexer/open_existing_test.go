package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/app"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

func TestOpenExistingMissingOwnerDoesNotStartOrCreateProfile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-not-create")
	var output, diagnostic bytes.Buffer
	if run(context.Background(), []string{"serve", "--open-existing", "--listen", "127.0.0.1:64165", "--data-dir", root, "--no-open"}, &output, &diagnostic) != 1 {
		t.Fatal("missing owner did not fail without starting")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("open-existing started or created application data")
	}
	if output.Len() != 0 || strings.Contains(diagnostic.String(), root) {
		t.Fatal("open-existing exposed private launch data")
	}
}

func TestOpenExistingOptionsRefuseConfigurationChanges(t *testing.T) {
	if _, err := parseArgs([]string{"serve", "--open-existing", "--no-open"}); err != nil {
		t.Fatal("attach-only launch refused")
	}
	for _, args := range [][]string{
		{"serve", "--open-existing", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "https://archive.example.com"},
		{"serve", "--open-existing", "--mcp-listen", "127.0.0.1:6968", "--mcp-public-url", "https://archive.example.com/mcp"},
		{"serve", "--open-existing", "--originals-budget", "64MiB"},
		{"serve", "--open-existing", "--demo", "--demo-acceptance-fixtures"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("attach-only launch silently ignored a configuration change")
		}
	}
}

func TestOpenExistingAuthenticatesOnceAndRejectsUntrustedOccupiedPort(t *testing.T) {
	for _, trusted := range []bool{true, false} {
		name := "untrusted"
		if trusted {
			name = "trusted"
		}
		t.Run(name, func(t *testing.T) {
			root := privateCLIDataRoot(t)
			application, err := app.New(context.Background(), app.Config{DataDir: root, Demo: true})
			if err != nil {
				t.Fatal(err)
			}
			defer application.Close(context.Background())
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			owner, err := localui.New(localui.Config{Origin: "http://" + listener.Addr().String(), Demo: true}, application)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.RevokeSessions()
			if application.WriteOwnerURL(owner.AttachURL()) != nil {
				t.Fatal("could not write private owner metadata")
			}
			var attempts atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/owner/attach" {
					attempts.Add(1)
				}
				if trusted {
					owner.Handler().ServeHTTP(w, r)
				} else {
					w.WriteHeader(http.StatusOK)
				}
			})
			server := &http.Server{Handler: handler}
			go server.Serve(listener)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var output, diagnostic bytes.Buffer
			code := run(ctx, []string{"serve", "--open-existing", "--listen", listener.Addr().String(), "--data-dir", root, "--demo", "--no-open"}, &output, &diagnostic)
			if attempts.Load() != 1 {
				t.Fatal("attach-only launch did not perform exactly one signed owner authentication")
			}
			if trusted {
				if code != 0 || !strings.Contains(output.String(), "already running") || diagnostic.Len() != 0 {
					t.Fatal("authenticated attach-only launch did not open the existing owner")
				}
			} else if code != 1 || output.Len() != 0 {
				t.Fatal("unauthenticated occupied port was accepted or a new app was started")
			}
		})
	}
}
