package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

// fakeKagent answers kagent-api tool calls as SSE, like the controller.
func fakeKagent(t *testing.T, state string) (*httptest.Server, *[]string) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		calls = append(calls, req.Params.Name)
		sb := map[string]any{"id": "sb1", "name": "agent-computer", "state": state, "operation": "RUNTIME_OPERATION_NONE",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339), "sandbox_template": map[string]string{"namespace": "kagent", "name": "mcp-desktop"}}
		var out any = sb
		if req.Params.Name == "list_sandboxes" {
			out = map[string]any{"sandboxes": []any{sb}}
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"structuredContent": out}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: " + string(b) + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestStatusMapsRuntimeState(t *testing.T) {
	for state, want := range map[string]string{"RUNTIME_STATE_READY": "awake", "RUNTIME_STATE_SUSPENDED": "asleep", "RUNTIME_STATE_CREATING": "starting"} {
		srv, calls := fakeKagent(t, state)
		c := &Computer{Kagent: &Kagent{URL: srv.URL, HTTP: srv.Client()}, Namespace: "kagent", Template: "mcp-desktop", Name: "agent-computer", Idle: time.Minute}
		st, err := c.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.State != want || st.SandboxID != "sb1" {
			t.Errorf("%s: got %+v, want %s", state, st, want)
		}
		if len(*calls) != 1 || (*calls)[0] != "list_sandboxes" {
			t.Errorf("status must only list, got %v", *calls) // never creates or wakes
		}
	}
}

func TestControlNeedsToken(t *testing.T) {
	srv, _ := fakeKagent(t, "RUNTIME_STATE_READY")
	c := &Computer{Kagent: &Kagent{URL: srv.URL, HTTP: srv.Client()}, Template: "mcp-desktop", Name: "agent-computer",
		Token: func() (string, error) { return "secret", nil }}
	for auth, want := range map[string]int{"": 401, "Bearer wrong": 401, "Bearer secret": 200} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/computer/status", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		c.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("auth %q: %d, want %d", auth, rec.Code, want)
		}
	}
}

func TestBrokenSandboxIsReplaced(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		calls = append(calls, req.Params.Name)
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"structuredContent": map[string]any{}}})
		w.Write(b)
	}))
	defer srv.Close()
	c := &Computer{Kagent: &Kagent{URL: srv.URL, HTTP: srv.Client()}, sb: &Sandbox{ID: "sb1"}}
	for i := 1; i < maxStartFailures; i++ {
		c.failed(context.Background(), errors.New("ingress failed"))
		if c.sb == nil {
			t.Fatalf("replaced after %d failures", i)
		}
	}
	c.failed(context.Background(), errors.New("ingress failed"))
	if c.sb != nil || len(calls) != 1 || calls[0] != "delete_sandbox" || !slices.Contains(c.replaced, "sb1") {
		t.Fatalf("sb=%v calls=%v replaced=%v", c.sb, calls, c.replaced)
	}
}
