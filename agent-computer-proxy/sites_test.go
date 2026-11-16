package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSitesLoadAndRender(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "allow-sites.txt"), []byte("# video\nyoutube.com\n*.ytimg.com\nhttps://github.com/\n=api.example.org\nyoutube.com\nlocalhost\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "allow-more.txt"), []byte("kagent.dev\n"), 0o644)
	// hosts-file style blocklist, as published DNS blocklists are
	os.WriteFile(filepath.Join(dir, "deny-ads.txt"), []byte("0.0.0.0 ads.youtube.com\n127.0.0.1 tracker.example.com # c\n0.0.0.0 0.0.0.0\n=exact.example.com\nnot a hosts line\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("example.org\n"), 0o644)
	l, err := (&Sites{Dir: dir}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"=api.example.org", "github.com", "kagent.dev", "youtube.com", "ytimg.com"}; !slices.Equal(l.Allow, want) {
		t.Fatalf("allow = %v", l.Allow)
	}
	if want := []string{"=exact.example.com", "ads.youtube.com", "tracker.example.com"}; !slices.Equal(l.Deny, want) {
		t.Fatalf("deny = %v", l.Deny)
	}
	origins := l.Origins()
	for _, o := range []string{"https://api.example.org", "https://youtube.com", "https://*.youtube.com", "https://*.ytimg.com"} {
		if !slices.Contains(origins, o) {
			t.Errorf("origins miss %s: %v", o, origins)
		}
	}
	if slices.Contains(origins, "https://*.api.example.org") {
		t.Error("an exact host must not get a wildcard")
	}
	var pol struct{ URLBlocklist []string }
	json.Unmarshal(l.ChromiumPolicy(), &pol)
	if want := []string{".exact.example.com", "ads.youtube.com", "tracker.example.com"}; !slices.Equal(pol.URLBlocklist, want) {
		t.Fatalf("blocklist = %v", pol.URLBlocklist)
	}
}

func TestApplyEgressServerSideApply(t *testing.T) {
	var got struct {
		method, path, query, ctype, auth string
		body                             map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.query = r.Method, r.URL.Path, r.URL.RawQuery
		got.ctype, got.auth = r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got.body)
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("tok\n"), 0o600)
	k := &Kube{Host: srv.URL, TokenFile: token}
	if err := k.ApplyEgress(context.Background(), "kagent", "agent-computer-web-egress", []string{"https://a.com", "https://*.a.com"}); err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPatch || got.path != "/apis/api.kagent.dev/v1alpha3/namespaces/kagent/egresspolicies/agent-computer-web-egress" ||
		!strings.Contains(got.query, "fieldManager=agent-computer-proxy") || got.ctype != "application/apply-patch+yaml" || got.auth != "Bearer tok" {
		t.Fatalf("request = %+v", got)
	}
	dests := got.body["spec"].(map[string]any)["destinations"].([]any)
	if len(dests) != 2 || dests[1].(map[string]any)["origin"] != "https://*.a.com" {
		t.Fatalf("destinations = %v", dests)
	}
}
