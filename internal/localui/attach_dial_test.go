package localui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAttachWithConnectKeepsApprovedBrowserAuthority(t *testing.T) {
	const origin = "http://127.0.0.1:9000"
	owner, err := New(Config{Origin: origin}, &fakeApplication{})
	if err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(owner.Handler())
	defer peer.Close()
	actual := strings.TrimPrefix(peer.URL, "http://")
	t.Setenv("HTTP_PROXY", "http://192.0.2.1:1234")
	t.Setenv("ALL_PROXY", "http://192.0.2.1:1234")
	first, err := AttachWithConnect(context.Background(), owner.AttachURL(), false, actual)
	if err != nil || !strings.HasPrefix(first, origin+"/#bootstrap=") {
		t.Fatal("split-port signed attach failed")
	}
	parsed, _ := url.Parse(first)
	token := strings.TrimPrefix(parsed.Fragment, "bootstrap=")
	if consumeAttachedBootstrap(owner, token).Code != 200 || consumeAttachedBootstrap(owner, token).Code != 401 {
		t.Fatal("bootstrap was not usable exactly once")
	}
	second, err := AttachWithConnect(context.Background(), owner.AttachURL(), false, actual)
	if err != nil || second == first {
		t.Fatal("repeat attach did not mint a fresh bootstrap")
	}
	if value, err := AttachWithConnect(context.Background(), owner.AttachURL(), true, actual); err != ErrOwnerAttach || value != "" {
		t.Fatal("wrong demo accepted")
	}
}

func TestAttachWithConnectRejectsUnsafeDialAndRedirects(t *testing.T) {
	owner := newTestServer(t, &fakeApplication{})
	for _, address := range []string{"", "localhost:8123", "0.0.0.0:8123", "192.0.2.1:8123", "127.0.0.1:0", "127.0.0.1:08123", "[::ffff:127.0.0.1]:8123", "[::1%lo0]:8123", "http://127.0.0.1:8123"} {
		value, err := AttachWithConnect(context.Background(), owner.AttachURL(), false, address)
		if err != ErrOwnerAttach || value != "" {
			t.Fatal("unsafe connect supplied browser authority")
		}
	}
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	value, err := AttachWithConnect(context.Background(), owner.AttachURL(), false, strings.TrimPrefix(redirect.URL, "http://"))
	if err != ErrOwnerAttach || value != "" || reached.Load() != 0 {
		t.Fatal("attach redirect accepted or followed")
	}
}
