package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// fakeBrowser is Chromium's DevTools endpoint: /json/version and a browser
// websocket that runs Storage.getCookies / setCookies on an in-memory jar. A
// cookie named "bad" is rejected; an expiry in the past deletes.
type fakeBrowser struct {
	mu  sync.Mutex
	jar map[string]cookie // domain|name|path
}

func (b *fakeBrowser) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": "ws://" + r.Host + "/devtools/browser/x"})
	})
	mux.HandleFunc("/devtools/browser/x", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.CloseNow()
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var m struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
				Params struct {
					Cookies []cookie `json:"cookies"`
				} `json:"params"`
			}
			json.Unmarshal(data, &m)
			c.Write(r.Context(), websocket.MessageText, []byte(`{"method":"Target.event","params":{}}`)) // events are skipped
			reply := map[string]any{"id": m.ID}
			b.mu.Lock()
			switch m.Method {
			case "Storage.getCookies":
				all := []cookie{}
				for _, v := range b.jar {
					all = append(all, v)
				}
				reply["result"] = map[string]any{"cookies": all}
			case "Storage.setCookies":
				bad := false
				for _, ck := range m.Params.Cookies {
					bad = bad || ck["name"] == "bad"
				}
				if bad {
					reply["error"] = map[string]any{"code": -32602, "message": "Invalid cookie fields"}
					break
				}
				for _, ck := range m.Params.Cookies {
					key := ck.domain() + "|" + ck["name"].(string) + "|" + ck["path"].(string)
					if e, ok := ck["expires"].(float64); ok && e > 0 && e < 1e9 {
						delete(b.jar, key)
						continue
					}
					ck["session"] = ck["expires"] == nil
					b.jar[key] = ck
				}
				reply["result"] = map[string]any{}
			default:
				reply["error"] = map[string]any{"code": -32601, "message": "unknown"}
			}
			b.mu.Unlock()
			out, _ := json.Marshal(reply)
			c.Write(r.Context(), websocket.MessageText, out)
		}
	})
	return mux
}

func cookieServer(t *testing.T, export string) (*httptest.Server, *fakeBrowser) {
	t.Helper()
	b := &fakeBrowser{jar: map[string]cookie{}}
	cdp := httptest.NewServer(b.handler(t))
	t.Cleanup(cdp.Close)
	mux := http.NewServeMux()
	mountCookies(mux, config{token: "secret", cdpURL: cdp.URL, exportToken: export})
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)
	return gw, b
}

func do(t *testing.T, method, url, token string, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func TestCookiesSetListDelete(t *testing.T) {
	gw, b := cookieServer(t, "")
	body := `{"cookies":[
		{"name":"s","value":"v1","domain":".github.com","path":"/","secure":true,"httpOnly":true,"expires":1900000000},
		{"name":"t","value":"v2","domain":"api.github.com","path":"/"},
		{"name":"bad","value":"v3","domain":"bad.com","path":"/"},
		{"name":"u","value":"v4","domain":"google.com","path":"/"}]}`
	if code, out := do(t, "POST", gw.URL+"/cookies", "", body); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s", code, out)
	}
	code, out := do(t, "POST", gw.URL+"/cookies", "secret", body)
	if code != 200 || !strings.Contains(out, `"set":3`) || !strings.Contains(out, `"domain":"bad.com"`) {
		t.Fatalf("POST: %d %s", code, out)
	}
	code, out = do(t, "GET", gw.URL+"/cookies/domains", "secret", "")
	if code != 200 || strings.Contains(out, "v1") {
		t.Fatalf("domains leaks values or failed: %d %s", code, out)
	}
	var domains map[string]int
	json.Unmarshal([]byte(out), &domains)
	if domains[".github.com"] != 1 || domains["api.github.com"] != 1 || domains["google.com"] != 1 || len(domains) != 3 {
		t.Errorf("domains = %v", domains)
	}
	code, out = do(t, "DELETE", gw.URL+"/cookies?domain=github.com", "secret", "")
	if code != 200 || !strings.Contains(out, `"deleted":2`) {
		t.Fatalf("DELETE: %d %s", code, out)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.jar) != 1 {
		t.Errorf("jar after delete: %v", b.jar)
	}
}

func TestCookiesLimits(t *testing.T) {
	gw, _ := cookieServer(t, "")
	big := `{"cookies":[{"name":"n","value":"` + strings.Repeat("x", maxCookieBody) + `","domain":"a.com","path":"/"}]}`
	if code, _ := do(t, "POST", gw.URL+"/cookies", "secret", big); code != http.StatusBadRequest {
		t.Errorf("2 MiB body: %d", code)
	}
	many := `{"cookies":[` + strings.TrimSuffix(strings.Repeat(`{"name":"n","domain":"a.com","path":"/"},`, maxCookiesPerPost+1), ",") + `]}`
	if code, _ := do(t, "POST", gw.URL+"/cookies", "secret", many); code != http.StatusRequestEntityTooLarge {
		t.Errorf("too many: %d", code)
	}
	if code, _ := do(t, "POST", gw.URL+"/cookies", "secret", `{"cookies":[{"name":"n"}]}`); code != http.StatusBadRequest {
		t.Errorf("no domain: %d", code)
	}
	if code, _ := do(t, "DELETE", gw.URL+"/cookies", "secret", ""); code != http.StatusBadRequest {
		t.Errorf("delete without domain: %d", code)
	}
}

func TestCookiesExportNeedsItsOwnToken(t *testing.T) {
	gw, _ := cookieServer(t, "")
	if code, _ := do(t, "GET", gw.URL+"/cookies/export", "secret", ""); code != http.StatusNotFound {
		t.Errorf("export without a configured token: %d", code)
	}
	gw, _ = cookieServer(t, "proxy-only")
	do(t, "POST", gw.URL+"/cookies", "secret", `{"cookies":[{"name":"s","value":"v1","domain":"a.com","path":"/","expires":1900000000}]}`)
	if code, _ := do(t, "GET", gw.URL+"/cookies/export", "secret", ""); code != http.StatusNotFound {
		t.Errorf("export with the viewer token: %d", code)
	}
	code, out := do(t, "GET", gw.URL+"/cookies/export", "proxy-only", "")
	if code != 200 || !strings.Contains(out, `"value":"v1"`) || !strings.Contains(out, `"expires":1900000000`) || strings.Contains(out, "session") {
		t.Errorf("export: %d %s", code, out)
	}
}

func TestMatchesSite(t *testing.T) {
	for _, c := range []struct {
		domain, site string
		want         bool
	}{{".github.com", "github.com", true}, {"api.github.com", "github.com", true}, {"notgithub.com", "github.com", false}, {"github.com", "", false}} {
		if got := matchesSite(c.domain, c.site); got != c.want {
			t.Errorf("matchesSite(%q, %q) = %v", c.domain, c.site, got)
		}
	}
}
