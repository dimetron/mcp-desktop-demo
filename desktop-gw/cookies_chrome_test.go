package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestCookiesRealChrome runs the /cookies routes against a real headless
// Chrome. Set GW_E2E_CHROME to its binary to run it.
func TestCookiesRealChrome(t *testing.T) {
	bin := os.Getenv("GW_E2E_CHROME")
	if bin == "" {
		t.Skip("set GW_E2E_CHROME to a Chrome binary")
	}
	// Chrome writes its profile while exiting, so clean up after it, not t.TempDir.
	profile, _ := os.MkdirTemp("", "gw-chrome-")
	defer os.RemoveAll(profile)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(bin, "--headless=new", "--no-first-run", "--no-default-browser-check",
		"--user-data-dir="+profile, fmt.Sprintf("--remote-debugging-port=%d", port), "about:blank")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	cdp := fmt.Sprintf("http://127.0.0.1:%d", port)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if resp, err := http.Get(cdp + "/json/version"); err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("chrome did not start")
		}
	}
	mux := http.NewServeMux()
	mountCookies(mux, config{token: "secret", cdpURL: cdp, exportToken: "exp"})
	gw := httptest.NewServer(mux)
	defer gw.Close()

	exp := time.Now().Add(48 * time.Hour).Unix()
	body := fmt.Sprintf(`{"cookies":[
		{"name":"user_session","value":"v1","domain":".github.com","path":"/","secure":true,"httpOnly":true,"sameSite":"Lax","expires":%d,"priority":"Medium","sourceScheme":"Secure","sourcePort":443},
		{"name":"sid","value":"v2","domain":"mail.google.com","path":"/mail","secure":true,"sameSite":"None"},
		{"name":"__Host-x","value":"bad","domain":".example.com","path":"/sub","secure":false}]}`, exp)
	code, out := do(t, "POST", gw.URL+"/cookies", "secret", body)
	t.Logf("POST: %d %s", code, out)
	if code != 200 || !strings.Contains(out, `"set":2`) || !strings.Contains(out, "example.com") {
		t.Fatalf("POST: %d %s", code, out)
	}
	code, out = do(t, "GET", gw.URL+"/cookies/domains", "secret", "")
	var domains map[string]int
	json.Unmarshal([]byte(out), &domains)
	t.Logf("domains: %v", domains)
	if code != 200 || domains[".github.com"] != 1 || domains["mail.google.com"] != 1 {
		t.Fatalf("domains: %d %s", code, out)
	}
	code, out = do(t, "GET", gw.URL+"/cookies/export", "exp", "")
	if code != 200 || !strings.Contains(out, `"httpOnly":true`) || !strings.Contains(out, fmt.Sprint(exp)) {
		t.Fatalf("export: %d %s", code, out)
	}
	code, out = do(t, "DELETE", gw.URL+"/cookies?domain=github.com", "secret", "")
	t.Logf("DELETE: %d %s", code, out)
	if code != 200 || !strings.Contains(out, `"deleted":1`) {
		t.Fatalf("DELETE: %d %s", code, out)
	}
	_, out = do(t, "GET", gw.URL+"/cookies/domains", "secret", "")
	if strings.Contains(out, "github.com") || !strings.Contains(out, "mail.google.com") {
		t.Fatalf("after delete: %s", out)
	}
}
