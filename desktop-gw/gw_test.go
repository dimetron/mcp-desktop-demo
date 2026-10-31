package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jezek/xgb/xproto"
)

// TestHelperProcess is a fake stdio MCP server used as the bridge child.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GW_FAKE_MCP") != "1" {
		return
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var m rpcMessage
		_ = json.Unmarshal(in.Bytes(), &m)
		if m.Method == "" || len(m.ID) == 0 {
			continue
		}
		// Exercise the paths the bridge must ignore or refuse.
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{}})
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": "srv-1", "method": "roots/list"})
		var result any
		switch m.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "fake", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(m.Params)}}}
		case "exit":
			os.Exit(0)
		default:
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
			continue
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	}
	os.Exit(0)
}

type mcpClient struct {
	t   *testing.T
	url string
	sid string
	id  int
}

func (c *mcpClient) post(method string, params any, notification bool) (*http.Response, map[string]any) {
	c.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if !notification {
		c.id++
		msg["id"] = c.id
	}
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	if c.sid != "" {
		req.Header.Set(headerSession, c.sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if s := resp.Header.Get(headerSession); s != "" {
		c.sid = s
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func result(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	r, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", m)
	}
	return r
}

func newTestServer(t *testing.T) (*httptest.Server, *bridge, *ticketStore) {
	t.Helper()
	exe, _ := os.Executable()
	t.Setenv("GW_FAKE_MCP", "1")
	b := newBridge(fmt.Sprintf("exec %q -test.run=TestHelperProcess", exe), 2, time.Hour, 5*time.Second)
	tickets := newTicketStore(time.Minute)
	b.app = &viewerApp{tickets: tickets, html: []byte("<html><head></head></html>")}
	srv := httptest.NewServer(requireToken("secret", b))
	t.Cleanup(func() { srv.Close(); b.closeAll() })
	return srv, b, tickets
}

var appsCaps = map[string]any{"capabilities": map[string]any{"extensions": map[string]any{
	appExtension: map[string]any{"mimeTypes": []string{appMimeType}}}}}

func TestBridgeMCPAppsFlow(t *testing.T) {
	srv, _, tickets := newTestServer(t)
	c := &mcpClient{t: t, url: srv.URL}

	_, init := c.post("initialize", appsCaps, false)
	if c.sid == "" {
		t.Fatal("initialize did not return a session id")
	}
	caps := result(t, init)["capabilities"].(map[string]any)
	if _, ok := caps["resources"]; !ok {
		t.Errorf("resources capability not added: %v", caps)
	}
	if resp, _ := c.post("notifications/initialized", nil, true); resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification status = %d", resp.StatusCode)
	}

	_, list := c.post("tools/list", map[string]any{}, false)
	var names []string
	for _, tool := range result(t, list)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "echo,"+viewerTool {
		t.Errorf("tools = %v", names)
	}

	_, res := c.post("resources/list", map[string]any{}, false)
	rs := result(t, res)["resources"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["uri"] != viewerURI {
		t.Errorf("resources = %v", rs)
	}

	_, read := c.post("resources/read", map[string]any{"uri": viewerURI}, false)
	content := result(t, read)["contents"].([]any)[0].(map[string]any)
	if content["mimeType"] != appMimeType || !strings.Contains(content["text"].(string), "<html>") {
		t.Errorf("viewer resource = %v", content)
	}
	csp := content["_meta"].(map[string]any)["ui"].(map[string]any)["csp"].(map[string]any)
	if got := csp["connectDomains"].([]any)[0]; got != "ws://"+strings.TrimPrefix(srv.URL, "http://") {
		t.Errorf("connectDomains = %v", got)
	}

	_, call := c.post("tools/call", map[string]any{"name": viewerTool, "arguments": map[string]any{"control": true}}, false)
	meta := result(t, call)["_meta"].(map[string]any)[ticketMetaKey].(map[string]any)
	if text := fmt.Sprint(result(t, call)["content"]); strings.Contains(text, meta["ticket"].(string)) {
		t.Error("ticket leaked into model-visible content")
	}
	tk, ok := tickets.redeem(meta["ticket"].(string))
	if !ok || !tk.control {
		t.Errorf("ticket redeem = %v %v", tk, ok)
	}

	_, echo := c.post("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"x": 1}}, false)
	if !strings.Contains(fmt.Sprint(result(t, echo)["content"]), `"x":1`) {
		t.Errorf("echo = %v", echo)
	}
}

func TestBridgeWithoutAppsHostHidesViewer(t *testing.T) {
	srv, _, _ := newTestServer(t)
	c := &mcpClient{t: t, url: srv.URL}
	c.post("initialize", map[string]any{"capabilities": map[string]any{}}, false)
	_, list := c.post("tools/list", map[string]any{}, false)
	if tools := result(t, list)["tools"].([]any); len(tools) != 1 {
		t.Errorf("viewer tool advertised to a host without MCP Apps: %v", tools)
	}
	_, call := c.post("tools/call", map[string]any{"name": viewerTool}, false)
	if strings.Contains(fmt.Sprint(call), ticketMetaKey) {
		t.Error("viewer tool answered locally for a host without MCP Apps")
	}
}

func TestBridgeProtocolErrors(t *testing.T) {
	srv, b, _ := newTestServer(t)
	do := func(method, sid, body string) int {
		req, _ := http.NewRequest(method, srv.URL, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		if sid != "" {
			req.Header.Set(headerSession, sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	tests := []struct {
		name, method, sid, body string
		want                    int
	}{
		{"batch", "POST", "", `[{"jsonrpc":"2.0","id":1,"method":"initialize"}]`, 400},
		{"not json-rpc", "POST", "", `{"id":1}`, 400},
		{"missing session", "POST", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 400},
		{"unknown session", "POST", "nope", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 404},
		{"sse stream", "GET", "", ``, 405},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := do(tt.method, tt.sid, tt.body); got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d", resp.StatusCode)
	}

	c1 := &mcpClient{t: t, url: srv.URL}
	c1.post("initialize", nil, false)
	c2 := &mcpClient{t: t, url: srv.URL}
	c2.post("initialize", nil, false)
	if resp, _ := (&mcpClient{t: t, url: srv.URL}).post("initialize", nil, false); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("third session status = %d, want 503", resp.StatusCode)
	}
	if got := do("DELETE", c1.sid, ""); got != 204 {
		t.Errorf("delete = %d", got)
	}
	if got := do("DELETE", c1.sid, ""); got != 404 {
		t.Errorf("second delete = %d", got)
	}

	// A child that exits fails the in-flight request and frees the session.
	resp, out := c2.post("exit", nil, false)
	if resp.StatusCode != 200 || out["error"] == nil {
		t.Errorf("exit call = %d %v", resp.StatusCode, out)
	}
	deadline := time.Now().Add(3 * time.Second)
	for b.lookup(c2.sid) != nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if b.lookup(c2.sid) != nil {
		t.Error("dead session was not removed")
	}
}

func TestTickets(t *testing.T) {
	s := newTicketStore(time.Minute)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	id := s.issue(true)
	if _, ok := s.redeem(id); !ok {
		t.Fatal("fresh ticket rejected")
	}
	if _, ok := s.redeem(id); ok {
		t.Fatal("ticket redeemed twice")
	}
	id = s.issue(false)
	now = now.Add(2 * time.Minute)
	if _, ok := s.redeem(id); ok {
		t.Fatal("expired ticket accepted")
	}
}

func TestBuildKeymapPrefersLowestColumn(t *testing.T) {
	// keycode 10: a A ; keycode 11: 1 exclam ; keycode 12: A (column 0, e.g. a remap)
	syms := []xproto.Keysym{'a', 'A', '1', '!', 'A', 0}
	m := buildKeymap(10, 2, syms)
	if m['a'].code != 10 || m['!'].code != 11 || m['!'].column != 1 {
		t.Errorf("keymap = %v", m)
	}
	if m['A'].code != 12 || m['A'].column != 0 {
		t.Errorf("A = %+v, want keycode 12 column 0", m['A'])
	}
	if _, ok := m[0]; ok {
		t.Error("NoSymbol mapped")
	}
}

func TestTranslateInput(t *testing.T) {
	km := map[uint32]keyBinding{0xff0d: {code: 36}}
	tests := []struct {
		name string
		ev   inputEvent
		want []fakeCall
	}{
		{"move clamps", inputEvent{T: "move", X: 5000, Y: -3}, []fakeCall{{xMotionNotify, 0, 1279, 0}}},
		{"left down", inputEvent{T: "button", B: 1, Down: true, X: 1, Y: 2}, []fakeCall{{xMotionNotify, 0, 1, 2}, {xButtonPress, 1, 1, 2}}},
		{"bad button", inputEvent{T: "button", B: 9}, nil},
		{"wheel up", inputEvent{T: "wheel", DY: -1}, []fakeCall{{xButtonPress, 4, 0, 0}, {xButtonRelease, 4, 0, 0}}},
		{"wheel zero", inputEvent{T: "wheel"}, nil},
		{"enter up", inputEvent{T: "key", Keysym: 0xff0d}, []fakeCall{{xKeyRelease, 36, 0, 0}}},
		{"unmapped key", inputEvent{T: "key", Keysym: 0x1234, Down: true}, nil},
		{"unknown type", inputEvent{T: "exec"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translateInput(tt.ev, 1280, 800, km)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBroadcasterStopsWithoutViewersAndKeepsLatest(t *testing.T) {
	n := 0
	src := func() ([]byte, bool, error) { n++; return []byte{byte(n)}, true, nil }
	b := newBroadcaster(src, time.Millisecond)
	frames, unsubscribe := b.subscribe()
	time.Sleep(30 * time.Millisecond)
	f := <-frames
	if len(frames) != 0 || f[0] < 2 {
		t.Errorf("expected only the latest frame, got %v with %d queued", f, len(frames))
	}
	unsubscribe()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		running := b.running
		b.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("capture loop still running without viewers")
}

func TestBroadcasterResendsStaticScreenAfterRestart(t *testing.T) {
	calls := 0
	src := func() ([]byte, bool, error) { calls++; return []byte{1}, calls == 1, nil }
	b := newBroadcaster(src, time.Millisecond)
	waitStopped := func() {
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
			b.mu.Lock()
			running := b.running
			b.mu.Unlock()
			if !running {
				return
			}
		}
		t.Fatal("capture loop still running without viewers")
	}
	frames, unsubscribe := b.subscribe()
	<-frames
	unsubscribe()
	waitStopped()
	frames, unsubscribe = b.subscribe()
	defer unsubscribe()
	select {
	case f := <-frames:
		if !bytes.Equal(f, []byte{1}) {
			t.Errorf("got frame %v", f)
		}
	case <-time.After(time.Second):
		t.Error("reconnecting viewer received no frame for an unchanged screen")
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"loopback without token", map[string]string{"GW_LISTEN": "127.0.0.1:1"}, true},
		{"public without token", map[string]string{"GW_LISTEN": "0.0.0.0:1"}, false},
		{"public with token", map[string]string{"GW_LISTEN": "0.0.0.0:1", "GW_TOKEN": "x"}, true},
		{"cert without key", map[string]string{"GW_TLS_CERT": "c"}, false},
		{"client CA without cert", map[string]string{"GW_TLS_CLIENT_CA": "ca"}, false},
		{"fps out of range", map[string]string{"GW_FPS": "60"}, false},
		{"quality not a number", map[string]string{"GW_JPEG_QUALITY": "high"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"GW_LISTEN", "GW_TOKEN", "GW_TOKEN_FILE", "GW_TLS_CERT", "GW_TLS_KEY", "GW_TLS_CLIENT_CA", "GW_FPS", "GW_JPEG_QUALITY"} {
				t.Setenv(k, tt.env[k])
			}
			if _, err := loadConfig(); (err == nil) != tt.ok {
				t.Errorf("err = %v, want ok=%t", err, tt.ok)
			}
		})
	}
}

func TestWSOriginTrustsForwardedOnlyBehindMTLS(t *testing.T) {
	r := httptest.NewRequest("GET", "http://10.0.0.5:8080/mcp", nil)
	r.Header.Set("X-Forwarded-Host", "evil.example")
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := (&viewerApp{}).wsOrigin(r); got != "ws://10.0.0.5:8080" {
		t.Errorf("untrusted origin = %s", got)
	}
	if got := (&viewerApp{trustForwarded: true}).wsOrigin(r); got != "wss://evil.example" {
		t.Errorf("trusted origin = %s", got)
	}
}
