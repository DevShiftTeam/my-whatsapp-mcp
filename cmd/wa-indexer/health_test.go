package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/app"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealthUsesApprovedHostAndSendsNoCapabilityBeforePairing(t *testing.T) {
	const origin = "http://127.0.0.1:9000"
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Host != "127.0.0.1:9000" || r.RequestURI != "/healthz" || r.Header.Get("Origin") != origin || r.Header.Get("Cookie") != "" || r.ContentLength > 0 {
			t.Error("health changed authority or sent private capability")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ready\n"))
	}))
	defer peer.Close()
	root, _ := helperPrivateRoot(t, origin)
	// Application initialization would refuse these unsafe/unreadable trees;
	// read-only health must inspect only the owner marker and private handoff.
	for _, dir := range []string{"models", "media"} {
		for i := 0; i < 128; i++ {
			if err := os.MkdirAll(filepath.Join(root, dir, strings.Repeat("deep", i%16), string(rune('a'+i))), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("/outside-SENTINEL", filepath.Join(root, dir, "must-not-traverse")); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	code := run(context.Background(), []string{"health", "--data-dir", root, "--connect", strings.TrimPrefix(peer.URL, "http://")}, &out, &errs)
	if code != 0 || out.String() != "ready\n" || errs.Len() != 0 {
		t.Fatal("read-only process health failed before pairing")
	}
	after, err := os.ReadDir(root)
	if err != nil || len(after) != len(before) {
		t.Fatal("health changed private storage")
	}
	for _, name := range []string{"archive/archive.db", "session/session.db"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatal("health constructed archive/session state")
		}
	}
}

func TestHealthRejectsRedirectsOversizedBodiesAndUnreadyProcess(t *testing.T) {
	for _, mode := range []string{"redirect", "oversized", "status", "wrong-body", "wrong-content-type"} {
		t.Run(mode, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				switch mode {
				case "redirect":
					http.Redirect(w, r, "http://192.0.2.1:1234/", 307)
				case "oversized":
					w.Write([]byte("ready\n" + strings.Repeat(" ", 8192)))
				case "status":
					w.WriteHeader(503)
					w.Write([]byte("ready\n"))
				case "wrong-body":
					w.Write([]byte("private-SENTINEL"))
				case "wrong-content-type":
					w.Header().Set("Content-Type", "text/html")
					w.Write([]byte("ready\n"))
				}
			}))
			defer peer.Close()
			root, _ := helperPrivateRoot(t, "http://127.0.0.1:9000")
			var out, errs bytes.Buffer
			if run(context.Background(), []string{"health", "--data-dir", root, "--connect", strings.TrimPrefix(peer.URL, "http://")}, &out, &errs) != 1 || out.Len() != 0 || strings.Contains(errs.String(), "SENTINEL") {
				t.Fatal("unready/unsafe health response accepted or leaked")
			}
		})
	}
}

func TestHelpersDoNotCreateMissingRoots(t *testing.T) {
	for _, command := range []string{"owner-link", "health"} {
		root := filepath.Join(t.TempDir(), "missing-SENTINEL")
		var out, errs bytes.Buffer
		if run(context.Background(), []string{command, "--data-dir", root}, &out, &errs) != 1 || out.Len() != 0 || strings.Contains(errs.String(), root) {
			t.Fatal("missing root helper did not fail privately")
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatal("helper created a data root")
		}
	}
}

func TestHealthBoundsPrivateReadsAndSelectedLoopbackDials(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ready\n"))
	}))
	defer peer.Close()
	root, _ := helperPrivateRoot(t, "http://127.0.0.1:9000")
	actual := strings.TrimPrefix(peer.URL, "http://")
	reads, dials := 0, 0
	operations := healthIO{
		readOwnerURL: func(cfg app.Config) (string, error) {
			reads++
			if cfg.DataDir != root {
				t.Error("unexpected private root")
			}
			return app.OwnerAttachURL(cfg)
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials++
			if address != actual || !strings.HasPrefix(address, "127.0.0.1:") {
				t.Error("health dial escaped selected loopback")
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 2*time.Second {
				t.Error("health has no bounded deadline")
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	var out bytes.Buffer
	if err := healthWithIO(context.Background(), options{dataDir: root, connect: actual}, &out, operations); err != nil || reads != 1 || dials != 1 || out.String() != "ready\n" {
		t.Fatal("health did not bound private reads/dials")
	}
	reads, dials = 0, 0
	if err := healthWithIO(context.Background(), options{dataDir: root, connect: "192.0.2.1:8123"}, &out, operations); !errors.Is(err, errHealth) || reads != 0 || dials != 0 {
		t.Fatal("unsafe connect accessed storage or network")
	}
}

func TestHealthTimesOutWithoutEchoingListenerBody(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer peer.Close()
	root, _ := helperPrivateRoot(t, "http://127.0.0.1:9000")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var out, errs bytes.Buffer
	start := time.Now()
	if run(ctx, []string{"health", "--data-dir", root, "--connect", strings.TrimPrefix(peer.URL, "http://")}, &out, &errs) != 1 || out.Len() != 0 || time.Since(start) > time.Second {
		t.Fatal("health ignored bounded cancellation")
	}
}

func TestHelperDefaultsSelectOnlyContainerLoopbackPort(t *testing.T) {
	for _, command := range []string{"owner-link", "health"} {
		opts, err := parseArgs([]string{command})
		if err != nil || opts.connect != "127.0.0.1:8123" || opts.command != command {
			t.Fatal("helper default did not select internal loopback port")
		}
	}
}

func TestContainerHelpersWorkBeforePairingAndRoutineLogsStayCapabilityFree(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	actual := listener.Addr().String()
	listener.Close()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "container-demo")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var routine, errs bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--container", "--listen", actual, "--browser-origin", "http://127.0.0.1:9000", "--data-dir", root, "--demo"}, &routine, &errs)
	}()
	t.Cleanup(func() { cancel() })
	ownerFile := filepath.Join(root, "owner-url")
	var private []byte
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		private, err = os.ReadFile(ownerFile)
		if err == nil {
			break
		}
		select {
		case <-done:
			t.Fatal("container process exited before readiness")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err != nil {
		cancel()
		<-done
		t.Fatal("container did not write its protected owner handoff")
	}
	var ready, login, helperErrors bytes.Buffer
	healthCode := run(context.Background(), []string{"health", "--data-dir", root, "--demo"}, &ready, &helperErrors)
	loginCode := run(context.Background(), []string{"owner-link", "--data-dir", root, "--demo"}, &login, &helperErrors)
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal("container shutdown failed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("container shutdown did not join")
	}
	if healthCode != 0 || ready.String() != "ready\n" || loginCode != 0 || !strings.HasPrefix(login.String(), "http://127.0.0.1:9000/#bootstrap=") || helperErrors.Len() != 0 {
		t.Fatal("container helpers did not qualify custom browser/internal port split")
	}
	for _, secret := range []string{"#attach=", "#bootstrap=", strings.TrimSpace(string(private)), strings.TrimSpace(login.String())} {
		if strings.Contains(routine.String()+errs.String(), secret) {
			t.Fatal("routine headless container logs disclosed a capability")
		}
	}
}
