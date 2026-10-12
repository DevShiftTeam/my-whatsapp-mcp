package localui

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestContainerListenerConfigurationRejectsUnsafeAddresses(t *testing.T) {
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "localhost:8123", "192.0.2.1:8123", "[::ffff:127.0.0.1]:8123", "[::1%lo0]:8123", "0.0.0.0:08123", ":8123"} {
		if _, err := New(Config{Origin: testOrigin, ContainerListen: address}, &fakeApplication{}); err == nil {
			t.Fatalf("accepted unsafe internal listener %q", address)
		}
	}
	for _, address := range []string{"0.0.0.0:8123", "127.0.0.1:8123", "[::]:8123", "[::1]:8123"} {
		if _, err := New(Config{Origin: testOrigin, ContainerListen: address}, &fakeApplication{}); err != nil {
			t.Fatalf("refused explicit internal listener %q: %v", address, err)
		}
	}
	for _, origin := range []string{"http://0.0.0.0:8123", "http://localhost:8123", "http://192.0.2.1:8123", "http://[::ffff:127.0.0.1]:8123"} {
		if _, err := New(Config{Origin: origin, ContainerListen: "0.0.0.0:8123"}, &fakeApplication{}); err == nil {
			t.Fatal("container widened browser authority")
		}
	}
}

func TestServeRejectsListenerOutsideConfiguredPolicy(t *testing.T) {
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, config := range []Config{{Origin: testOrigin}, {Origin: testOrigin, ContainerListen: "127.0.0.1:" + strings.Split(listener.Addr().String(), ":")[1]}, {Origin: testOrigin, ContainerListen: "0.0.0.0:1"}} {
		owner := newTestServer(t, &fakeApplication{}, config)
		if err := owner.Serve(context.Background(), listener); err == nil {
			t.Fatal("mismatched listener accepted")
		}
	}
}

func TestWildcardListenerPreservesBrowserAuthorityAuthenticationAndCSRF(t *testing.T) {
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	app := &fakeApplication{}
	owner := newTestServer(t, app, Config{Origin: testOrigin, ContainerListen: listener.Addr().String()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- owner.Serve(ctx, listener) }()
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strings.Split(listener.Addr().String(), ":")[1]))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	send := func(path string, session *testSession, host string, origins []string, csrf string, body string) *http.Response {
		t.Helper()
		r, err := http.NewRequest(http.MethodPost, testOrigin+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Host = host
		r.Header.Set("Content-Type", "application/json")
		for _, origin := range origins {
			r.Header.Add("Origin", origin)
		}
		r.Header.Set("X-Forwarded-Host", "127.0.0.1:44771")
		r.Header.Set("Forwarded", "host=127.0.0.1:44771")
		if session != nil {
			r.AddCookie(session.cookie)
		}
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		reply, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { reply.Body.Close() })
		return reply
	}
	u, _ := url.Parse(owner.BootstrapURL())
	body, _ := json.Marshal(map[string]string{"token": strings.TrimPrefix(u.Fragment, "bootstrap=")})
	reply := send("/api/bootstrap", nil, "127.0.0.1:44771", []string{testOrigin}, "", string(body))
	if reply.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap status %d", reply.StatusCode)
	}
	var bootstrap struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(reply.Body).Decode(&bootstrap); err != nil {
		t.Fatal(err)
	}
	session := testSession{cookie: reply.Cookies()[0], csrf: bootstrap.CSRF}
	for _, tc := range []struct {
		name, host string
		origins    []string
		session    *testSession
		csrf       string
		status     int
	}{
		{"missing authentication", "127.0.0.1:44771", []string{testOrigin}, nil, session.csrf, http.StatusUnauthorized},
		{"missing CSRF", "127.0.0.1:44771", []string{testOrigin}, &session, "", http.StatusForbidden},
		{"container IP", "172.18.0.2:8123", []string{testOrigin}, &session, session.csrf, http.StatusForbidden},
		{"wildcard host", "0.0.0.0:8123", []string{testOrigin}, &session, session.csrf, http.StatusForbidden},
		{"foreign origin", "127.0.0.1:44771", []string{"http://evil.example"}, &session, session.csrf, http.StatusForbidden},
		{"multiple origins", "127.0.0.1:44771", []string{testOrigin, testOrigin}, &session, session.csrf, http.StatusForbidden},
		{"absent origin", "127.0.0.1:44771", nil, &session, session.csrf, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := app.total()
			reply := send("/api/pause", tc.session, tc.host, tc.origins, tc.csrf, "{}")
			io.Copy(io.Discard, reply.Body)
			if reply.StatusCode != tc.status {
				t.Fatalf("status %d want %d", reply.StatusCode, tc.status)
			}
			assertNoCalls(t, app, before)
		})
	}
	reply = send("/api/pause", &session, "127.0.0.1:44771", []string{testOrigin}, session.csrf, "{}")
	io.Copy(io.Discard, reply.Body)
	if reply.StatusCode != http.StatusOK || app.count("pause") != 1 {
		t.Fatal("authenticated browser mutation rejected")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("container listener shutdown did not finish")
	}
}
