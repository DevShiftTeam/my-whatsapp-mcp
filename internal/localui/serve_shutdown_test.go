package localui

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"net"
	"net/http"
	"testing"
	"time"
)

type blockedNativeApplication struct {
	*fakeApplication
	entered, release chan struct{}
	cooperative      bool
}

func (a *blockedNativeApplication) Status(ctx context.Context) (archive.StatusSnapshot, error) {
	close(a.entered)
	if a.cooperative {
		<-ctx.Done()
	} else {
		<-a.release
	}
	return archive.StatusSnapshot{}, nil
}
func TestNativeShutdownSharesBudgetAndRequiresRequestJoin(t *testing.T) {
	for _, cooperative := range []bool{true, false} {
		t.Run(map[bool]string{true: "cooperative", false: "noncooperative"}[cooperative], func(t *testing.T) {
			l, e := net.Listen("tcp4", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer l.Close()
			a := &blockedNativeApplication{fakeApplication: &fakeApplication{}, entered: make(chan struct{}), release: make(chan struct{}), cooperative: cooperative}
			s, e := New(Config{Origin: "http://" + l.Addr().String()}, a)
			if e != nil {
				t.Fatal(e)
			}
			s.owner = &session{digest: sha256.Sum256([]byte("synthetic-cookie")), csrf: "synthetic-csrf", expires: time.Now().Add(time.Hour)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			budget, stop := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer stop()
			callback := make(chan struct{}, 1)
			result := make(chan error, 1)
			go func() {
				result <- s.ServeWithShutdown(ctx, l, func() context.Context { callback <- struct{}{}; return budget })
			}()
			requestDone := make(chan struct{})
			go func() {
				defer close(requestDone)
				r, _ := http.NewRequest("GET", s.origin+"/api/status", nil)
				r.AddCookie(&http.Cookie{Name: s.cookieName, Value: "synthetic-cookie"})
				r.Header.Set("X-CSRF-Token", "synthetic-csrf")
				client := &http.Client{Timeout: time.Second}
				res, e := client.Do(r)
				if e == nil {
					res.Body.Close()
				}
			}()
			select {
			case <-a.entered:
			case <-time.After(time.Second):
				t.Fatal("request not entered")
			}
			start := time.Now()
			cancel()
			select {
			case e := <-result:
				if cooperative && e != nil {
					t.Fatal(e)
				}
				if !cooperative && !errors.Is(e, ErrShutdownIncomplete) {
					t.Fatal("active native request treated as joined", e)
				}
			case <-time.After(time.Second):
				t.Fatal("shutdown unbounded")
			}
			if time.Since(start) > 500*time.Millisecond {
				t.Fatal("fresh timeout substituted")
			}
			select {
			case <-callback:
			default:
				t.Fatal("shared budget unused")
			}
			close(a.release)
			<-requestDone
		})
	}
}
