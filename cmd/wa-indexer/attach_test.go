package main

import (
	"bytes"
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/app"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecondLaunchAuthenticatesFreshModeWithoutPrintingCapability(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	application, err := app.New(context.Background(), app.Config{DataDir: privateCLIDataRoot(t), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close(context.Background())
	server, err := localui.New(localui.Config{Origin: "http://" + listener.Addr().String(), Demo: true}, application)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: server.Handler()}
	go httpServer.Serve(listener)
	defer httpServer.Close()
	var output bytes.Buffer
	if err = attachExisting(context.Background(), server.AttachURL(), true, true, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), listener.Addr().String()) || strings.Contains(output.String(), "attach=") || strings.Contains(output.String(), "bootstrap=") {
		t.Fatal("attach capability leaked")
	}
	if err = attachExisting(context.Background(), server.AttachURL(), false, true, &output); err == nil {
		t.Fatal("real/demo mismatch attached")
	}
	server.RevokeSessions()
	if err = attachExisting(context.Background(), server.AttachURL(), true, true, &output); err == nil {
		t.Fatal("revoked owner capability attached")
	}
}

func privateCLIDataRoot(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(base, "private-data")
}

func TestServeOccupiedFixedListenerAuthenticatesExistingOwner(t *testing.T) {
	root := privateCLIDataRoot(t)
	application, err := app.New(context.Background(), app.Config{DataDir: root, Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close(context.Background())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server, err := localui.New(localui.Config{Origin: "http://" + listener.Addr().String(), Demo: true}, application)
	if err != nil {
		t.Fatal(err)
	}
	defer server.RevokeSessions()
	if err = application.WriteOwnerURL(server.AttachURL()); err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: server.Handler()}
	go httpServer.Serve(listener)
	defer httpServer.Close()
	var output, diagnostic bytes.Buffer
	if err = serve(context.Background(), options{listen: listener.Addr().String(), dataDir: root, demo: true, noOpen: true}, &output, &diagnostic); err != nil {
		t.Fatal("occupied original listener did not attach")
	}
	if !strings.Contains(output.String(), "already running") || diagnostic.Len() != 0 {
		t.Fatal("attached relaunch returned wrong readiness")
	}
	if err = serve(context.Background(), options{listen: listener.Addr().String(), dataDir: root, demo: false, noOpen: true}, &output, &diagnostic); err == nil {
		t.Fatal("occupied port bypassed expected-mode authentication")
	}
}
