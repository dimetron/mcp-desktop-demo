package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gwClient dials srv for every request, like the tunnel to one actor.
func gwClient(srv *httptest.Server) *http.Client {
	addr := strings.TrimPrefix(srv.URL, "http://")
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}}
}

func TestCarryCookies(t *testing.T) {
	var exported []map[string]any
	for i := range 4500 {
		exported = append(exported, map[string]any{"name": "n", "value": strings.Repeat("v", i%7), "domain": "a.com", "path": "/"})
	}
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != exportPath || r.Header.Get("Authorization") != "Bearer export-tok" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"cookies": exported})
	}))
	defer old.Close()
	var posts, got int
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/cookies" || r.Header.Get("Authorization") != "Bearer viewer-tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		var body struct{ Cookies []json.RawMessage }
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Cookies) > handoffBatch {
			t.Errorf("batch of %d", len(body.Cookies))
		}
		posts++
		got += len(body.Cookies)
		json.NewEncoder(w).Encode(map[string]int{"set": len(body.Cookies)})
	}))
	defer fresh.Close()
	n, err := carryCookies(context.Background(), gwClient(old), "export-tok", gwClient(fresh), "viewer-tok")
	if err != nil || n != 4500 || got != 4500 || posts != 3 {
		t.Fatalf("n=%d err=%v got=%d posts=%d", n, err, got, posts)
	}
	if _, err := carryCookies(context.Background(), gwClient(old), "wrong", gwClient(fresh), "viewer-tok"); err == nil {
		t.Error("want an error with the wrong export token")
	}
}

func TestExportRouteNotProxied(t *testing.T) {
	c := &Computer{Token: func() (string, error) { return "secret", nil }}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, exportPath, nil)
	req.Header.Set("Authorization", "Bearer secret")
	c.ServeHTTP(rec, req) // would panic reaching Ensure with no Kagent
	if rec.Code != http.StatusNotFound {
		t.Errorf("export from outside: %d", rec.Code)
	}
}

func TestExportTokenPerSandbox(t *testing.T) {
	c := &Computer{}
	a, b := c.exportTokenFor("sb1"), c.exportTokenFor("sb2")
	if a == "" || a == b || c.exportTokenFor("sb1") != a {
		t.Errorf("tokens %q %q", a, b)
	}
}
