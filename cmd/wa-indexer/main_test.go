package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/app"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

type opaqueStartupError struct {
	cause     error
	formatted bool
}

func (e *opaqueStartupError) Error() string {
	e.formatted = true
	return "PRIVATE_STARTUP_SENTINEL /private/data TOKEN"
}

func (e *opaqueStartupError) Unwrap() error { return e.cause }

func TestStartupErrorMessagesUseFixedClassesWithoutFormattingErrors(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		code  string
	}{
		{"private data path", appdata.ErrUnsafeDataPath, "unsafe_data_path"},
		{"private archive path", archive.ErrUnsafeDataPath, "unsafe_data_path"},
		{"data unavailable", appdata.ErrDataUnavailable, "data_unavailable"},
		{"ownership lock", appdata.ErrLockRequired, "instance_lock_required"},
		{"existing app", appdata.ErrInstanceAlreadyRunning, "instance_already_running"},
		{"existing archive", archive.ErrInstanceAlreadyRunning, "instance_already_running"},
		{"archive unavailable", archive.ErrArchiveUnavailable, "archive_unavailable"},
		{"WhatsApp unavailable", archive.ErrSourceUnavailable, "source_unavailable"},
		{"interrupted import", archive.ErrReconciliationRequired, "reconciliation_required"},
		{"newer schema", archive.ErrSchemaTooNew, "schema_too_new"},
		{"changed schema", archive.ErrSchemaChanged, "schema_changed"},
		{"missing FTS5", archive.ErrFTS5Unavailable, "fts5_unavailable"},
		{"wrong account", archive.ErrUnauthorized, "unauthorized"},
		{"unsupported platform", appdata.ErrUnsupportedPlatform, "unsupported_platform"},
		{"canceled", context.Canceled, "canceled"},
		{"deadline", context.DeadlineExceeded, "deadline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			private := &opaqueStartupError{cause: tc.cause}
			message := startupErrorMessage(private)
			if !strings.Contains(message, "("+tc.code+")") {
				t.Fatalf("startup message omitted fixed class %s", tc.code)
			}
			if private.formatted || strings.Contains(message, "PRIVATE_STARTUP_SENTINEL") || strings.Contains(message, "/private/data") || strings.Contains(message, "TOKEN") {
				t.Fatal("startup feedback formatted or exposed an opaque error")
			}
		})
	}
	private := &opaqueStartupError{}
	if message := startupErrorMessage(private); private.formatted || message != startupErrorMessage(errStartup) {
		t.Fatal("unknown startup error was formatted or did not use generic feedback")
	}
}

func TestRunInvalidOptionsDoesNotExposeSuppliedValues(t *testing.T) {
	const secret = "private-argument-SENTINEL"
	var stdout, stderr bytes.Buffer
	if run(context.Background(), []string{"serve", "--demo=" + secret}, &stdout, &stderr) != 2 {
		t.Fatal("invalid options did not fail")
	}
	if stdout.Len() != 0 || stderr.Len() == 0 || strings.Contains(stderr.String(), secret) {
		t.Fatal("invalid options exposed their value or did not report an error")
	}
}

func TestRunUnsafeListenerDoesNotCreateData(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private-data-SENTINEL")
	var stdout, stderr bytes.Buffer
	if run(context.Background(), []string{"serve", "--listen", "0.0.0.0:0", "--data-dir", root, "--demo", "--no-open"}, &stdout, &stderr) != 1 {
		t.Fatal("unsafe listener did not fail")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsafe listener touched application storage")
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), root) || strings.Contains(stderr.String(), "0.0.0.0") {
		t.Fatal("listener error exposed a private path or supplied address")
	}
}

func TestRunStartupFailureDoesNotExposePrivatePath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "unrelated-SENTINEL"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if run(context.Background(), []string{"serve", "--listen", "127.0.0.1:0", "--data-dir", root, "--demo", "--no-open"}, &stdout, &stderr) != 1 {
		t.Fatal("unowned data directory was accepted")
	}
	if stdout.Len() != 0 || stderr.Len() == 0 || strings.Contains(stderr.String(), root) || strings.Contains(stderr.String(), "SENTINEL") {
		t.Fatal("startup error exposed local storage details")
	}
	if !strings.Contains(stderr.String(), "(unsafe_data_path)") {
		t.Fatal("real unsafe-folder startup failure lost its actionable class")
	}
}

func TestServeFixedListenerRejectsAuthenticatedOwnerOnDifferentPort(t *testing.T) {
	for _, occupied := range []bool{false, true} {
		name := "available listener"
		if occupied {
			name = "occupied listener"
		}
		t.Run(name, func(t *testing.T) {
			root := privateCLIDataRoot(t)
			application, err := app.New(context.Background(), app.Config{DataDir: root, Demo: true})
			if err != nil {
				t.Fatal(err)
			}
			defer application.Close(context.Background())
			owner, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			server, err := localui.New(localui.Config{Origin: "http://" + owner.Addr().String(), Demo: true}, application)
			if err != nil {
				t.Fatal(err)
			}
			defer server.RevokeSessions()
			if err := application.WriteOwnerURL(server.AttachURL()); err != nil {
				t.Fatal(err)
			}
			httpServer := &http.Server{Handler: server.Handler()}
			go httpServer.Serve(owner)
			defer httpServer.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := localui.Attach(ctx, server.AttachURL(), true); err != nil {
				t.Fatal("fixture owner did not authenticate before the fixed-port launch")
			}
			requested, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer requested.Close()
			address := requested.Addr().String()
			if !occupied {
				if err := requested.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var output, diagnostic bytes.Buffer
			err = serve(ctx, options{listen: address, dataDir: root, demo: true, noOpen: true}, &output, &diagnostic)
			if !errors.Is(err, errStartup) || output.Len() != 0 || diagnostic.Len() != 0 {
				t.Fatal("fixed-port launch silently attached to an owner on another port")
			}
			if !occupied {
				_, ownerPort, err := net.SplitHostPort(owner.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				for _, address := range []string{"127.0.0.1:0", "[127.0.0.1]:0", "[127.0.0.1]:" + ownerPort} {
					output.Reset()
					err = serve(ctx, options{listen: address, dataDir: root, demo: true, noOpen: true}, &output, &diagnostic)
					if err != nil || !strings.Contains(output.String(), "already running") || diagnostic.Len() != 0 {
						t.Errorf("accepted listener syntax did not authenticate the existing owner: %s", address)
					}
				}
			}
		})
	}
}

func TestRunDemoKeepsOwnerURLPrivateAndClosesResources(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("owned application storage is not supported on this platform")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "private-demo-SENTINEL")
	openerName := "xdg-open"
	if runtime.GOOS == "darwin" {
		openerName = "open"
	}
	openerDir := t.TempDir()
	openerLog := filepath.Join(openerDir, "opener-invoked")
	writeTestExecutable(t, filepath.Join(openerDir, openerName), "#!/bin/sh\nprintf invoked > \"$WA_INDEXER_TEST_OPENER_LOG\"\nexit 81\n")
	t.Setenv("WA_INDEXER_TEST_OPENER_LOG", openerLog)
	t.Setenv("PATH", openerDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestDemoLifecycleSubprocess$")
	command.Env = append(os.Environ(), "WA_INDEXER_CLI_HELPER_ROOT="+root)
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		t.Fatal("could not start the application lifecycle subprocess")
	}
	done := make(chan int, 1)
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = command.Process.Kill()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("application did not stop during test cleanup")
			}
		}
	})
	go func() {
		err := command.Wait()
		if err == nil {
			done <- 0
		} else if exit, ok := err.(*exec.ExitError); ok {
			done <- exit.ExitCode()
		} else {
			done <- -1
		}
	}()
	ownerFile := filepath.Join(root, "owner-url")
	var ownerURL []byte
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ownerURL, err = os.ReadFile(ownerFile)
		if err == nil {
			break
		}
		select {
		case code := <-done:
			finished = true
			t.Fatalf("application stopped before creating its private owner link: exit=%d", code)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("application did not create its private owner link")
	}
	info, err := os.Stat(ownerFile)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("owner handoff is not private")
	}
	parsed, err := url.Parse(strings.TrimSpace(string(ownerURL)))
	if err != nil || parsed.Fragment == "" {
		t.Fatal("private handoff is not a bootstrap link")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(parsed.Scheme + "://" + parsed.Host + "/")
	if err != nil {
		t.Fatal("application interface is not reachable")
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("application interface is unavailable")
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal("could not signal application shutdown")
	}
	select {
	case code := <-done:
		finished = true
		if code != 0 {
			t.Fatalf("normal shutdown failed: exit=%d", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("application shutdown exceeded its bound")
	}
	if stdout.String() != "My WhatsApp is running. Open the private owner link on this computer.\n" || stderr.Len() != 0 {
		t.Fatal("application output did not preserve its private-link contract")
	}
	if _, err := os.Stat(openerLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("--no-open launched a browser")
	}
	if strings.Contains(stdout.String()+stderr.String(), root) || strings.Contains(stdout.String()+stderr.String(), parsed.Fragment) || strings.Contains(stdout.String()+stderr.String(), parsed.Host) {
		t.Fatal("application printed a private path, port or bootstrap capability")
	}
	if _, err := os.Stat(ownerFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("shutdown retained a stale owner capability")
	}
	connection, err := net.DialTimeout("tcp", parsed.Host, 100*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatal("shutdown retained a listening interface")
	}
	paths, err := appdata.Prepare(root)
	if err != nil {
		t.Fatal("shutdown left the data root unusable")
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal("shutdown retained its application lease")
	}
	lease.Close()
}

// The helper calls the production signal entry point and exits before the test
// harness can append output, so the parent verifies the actual CLI contract.
func TestDemoLifecycleSubprocess(t *testing.T) {
	root := os.Getenv("WA_INDEXER_CLI_HELPER_ROOT")
	if root == "" {
		return
	}
	os.Args = []string{"wa-indexer", "serve", "--listen", "127.0.0.1:0", "--data-dir", root, "--demo", "--no-open"}
	os.Exit(signalMain())
}
