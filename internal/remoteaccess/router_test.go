package remoteaccess

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouterBoundary(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202) })
	for _, browser := range []bool{false, true} {
		router, err := newRouter("https://saved.ngrok-free.app", browser, Handlers{MCP: h, Owner: h})
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range []string{"/mcp", "/", "/assets/app.js", "/api/remote/login", "/api/chats?limit=10", "/api/bootstrap", "/api/owner/attach", "/bootstrap", "/attach", "/mcp/", "//mcp", "/%6dcp", "/mcp?x=1", "/mcp?", "/api/status/", "/api/remote/configure", "/api/interface", "/api/connections/reveal", "/api/remote/password", "/api/data/delete"} {
			req := httptest.NewRequest("GET", target, nil)
			req.Host = "saved.ngrok-free.app"
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			allowed := target == "/mcp" || browser && (target == "/" || target == "/assets/app.js" || target == "/api/remote/login" || target == "/api/interface" || target == "/api/chats?limit=10")
			want := 404
			if allowed {
				want = 202
			}
			if response.Code != want {
				t.Errorf("browser=%v target=%s code=%d want=%d", browser, target, response.Code, want)
			}
		}
		for _, host := range []string{"evil.example", "saved.ngrok-free.app:443", "saved.ngrok-free.app.", "SAVED.ngrok-free.app", "saved.ngrok-free.app@evil.example"} {
			req := httptest.NewRequest("GET", "/mcp", nil)
			req.Host = host
			req.Header.Set("X-Forwarded-Host", "saved.ngrok-free.app")
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			if resp.Code != 403 {
				t.Fatal("bad Host accepted")
			}
		}
		for _, origins := range [][]string{{"https://evil.example"}, {"https://saved.ngrok-free.app/"}, {"https://saved.ngrok-free.app", "https://saved.ngrok-free.app"}, {"null"}, {""}, {"https://saved.ngrok-free.app, https://evil.example"}} {
			req := httptest.NewRequest("GET", "/mcp", nil)
			req.Host = "saved.ngrok-free.app"
			req.Header["Origin"] = origins
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			if resp.Code != 403 {
				t.Fatal("bad Origin accepted")
			}
		}
		req := httptest.NewRequest("GET", "/mcp", nil)
		req.Host = "saved.ngrok-free.app"
		req.Header.Set("Origin", "https://saved.ngrok-free.app")
		req.Header.Set("Forwarded", "host=evil.example;proto=http")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != 202 {
			t.Fatal("exact Origin rejected")
		}
		if req.Header.Get("Origin") != "https://saved.ngrok-free.app" {
			t.Fatal("Origin rewritten")
		}
	}
}

// Public provider ingress must retain main's bearer send and opaque attachment
// transport even without the optional owner browser. Cookies never grant these
// handlers authority, and aliases never reach either handler.
func TestRouterPreservesMainMCPTransports(t *testing.T) {
	const origin = "https://saved.ngrok-free.app"
	attachment := "/mcp/attachments/" + strings.Repeat("A", 43)
	paths := []string{"/mcp", "/api/send/message", "/api/send/media", "/api/send/voice", "/api/send/status", attachment}
	for _, browser := range []bool{false, true} {
		mcpCalls, ownerCalls := 0, 0
		mcp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mcpCalls++
			if r.Header.Get("Authorization") != "Bearer synthetic" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		})
		owner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ownerCalls++; w.WriteHeader(http.StatusUnauthorized) })
		router, err := newRouter(origin, browser, Handlers{MCP: mcp, Owner: owner})
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			t.Run(path+"/browser="+map[bool]string{false: "off", true: "on"}[browser], func(t *testing.T) {
				for _, bearer := range []bool{false, true} {
					req := httptest.NewRequest(http.MethodPost, path, nil)
					req.Host = "saved.ngrok-free.app"
					req.Header.Set("Origin", origin)
					req.Header.Set("Cookie", "owner=synthetic")
					if bearer {
						req.Header.Set("Authorization", "Bearer synthetic")
					}
					resp := httptest.NewRecorder()
					router.ServeHTTP(resp, req)
					want := http.StatusUnauthorized
					if bearer {
						want = http.StatusAccepted
					}
					if resp.Code != want {
						t.Fatalf("bearer=%v: code=%d want=%d", bearer, resp.Code, want)
					}
				}
				for _, target := range []string{path + "/", path + "?x=1", path + "?", "/" + path, strings.Replace(path, "/", "/%2f", 1)} {
					before := mcpCalls
					req := httptest.NewRequest(http.MethodPost, target, nil)
					req.Host = "saved.ngrok-free.app"
					req.Header.Set("Authorization", "Bearer synthetic")
					resp := httptest.NewRecorder()
					router.ServeHTTP(resp, req)
					if resp.Code != http.StatusNotFound || before != mcpCalls {
						t.Fatalf("alias admitted: %s code=%d", target, resp.Code)
					}
				}
				for _, authority := range []struct{ host, origin string }{{"evil.example", origin}, {"saved.ngrok-free.app", "https://evil.example"}} {
					before := mcpCalls
					req := httptest.NewRequest(http.MethodPost, path, nil)
					req.Host = authority.host
					req.Header.Set("Origin", authority.origin)
					req.Header.Set("Authorization", "Bearer synthetic")
					resp := httptest.NewRecorder()
					router.ServeHTTP(resp, req)
					if resp.Code != http.StatusForbidden || before != mcpCalls {
						t.Fatal("wrong authority admitted")
					}
				}
			})
		}
		if ownerCalls != 0 || mcpCalls != len(paths)*2 {
			t.Fatalf("wrong transport: mcp=%d owner=%d", mcpCalls, ownerCalls)
		}
		for _, target := range []string{"/mcp/attachments/short", "/api/send/other", "/api/remote/status", "/api/remote/sign-in", "/api/remote/remove", "/api/remote/disable", "/api/connections/reveal", "/api/data/delete"} {
			req := httptest.NewRequest(http.MethodPost, target, nil)
			req.Host = "saved.ngrok-free.app"
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			if resp.Code != http.StatusNotFound {
				t.Fatalf("native-only or malformed route admitted: %s", target)
			}
		}
	}
}

func TestRouterPreservesMainOwnerAssetsAndEverydayAuth(t *testing.T) {
	mcp := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("owner path reached MCP") })
	owner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	for _, browser := range []bool{false, true} {
		router, err := newRouter("https://saved.ngrok-free.app", browser, Handlers{MCP: mcp, Owner: owner})
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/assets/app.js", "/assets/app.css", "/assets/theme.js", "/assets/selects.js", "/assets/logo.png", "/api/stats", "/api/voice/retry", "/api/voice/recovery", "/api/voice/recovery?chat_id=synthetic-chat", "/api/documents/retry", "/api/connections/send-permission"} {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Host = "saved.ngrok-free.app"
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			want := http.StatusNotFound
			if browser {
				want = http.StatusUnauthorized
				if strings.HasPrefix(path, "/assets/") {
					want = http.StatusOK
				}
			}
			if resp.Code != want {
				t.Fatalf("browser=%v path=%s code=%d want=%d", browser, path, resp.Code, want)
			}
		}
	}
}
