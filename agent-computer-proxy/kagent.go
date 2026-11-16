package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Kagent calls the controller's MCP server (kagent-api), whose sandbox tools
// are the only way to start processes in a kagent sandbox.
type Kagent struct {
	URL  string // e.g. http://kagent-controller.kagent.svc.cluster.local:8083/mcp
	HTTP *http.Client
	id   atomic.Int64
}

// Sandbox is the part of a kagent sandbox this proxy uses.
type Sandbox struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	State     string    `json:"state"` // RUNTIME_STATE_READY, _SUSPENDED, _CREATING, ...
	Operation string    `json:"operation"`
	ExpiresAt time.Time `json:"expires_at"`
	// Failure is set when the runtime can't carry out a lifecycle operation,
	// e.g. the actor crashed (Substrate killed it over its memory limit).
	Failure *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"failure"`
	Template struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"sandbox_template"`
}

// Call runs one tool and decodes its structuredContent into out.
func (k *Kagent) Call(ctx context.Context, tool string, args map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": k.id.Add(1), "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := k.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s %s", tool, resp.Status, bytes.TrimSpace(b))
	}
	payload, err := rpcPayload(resp)
	if err != nil {
		return fmt.Errorf("%s: %w", tool, err)
	}
	var msg struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
			Content           []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return fmt.Errorf("%s: %w", tool, err)
	}
	if msg.Error != nil {
		return fmt.Errorf("%s: %s", tool, msg.Error.Message)
	}
	if msg.Result.IsError {
		text := ""
		if len(msg.Result.Content) > 0 {
			text = msg.Result.Content[0].Text
		}
		return fmt.Errorf("%s: %s", tool, text)
	}
	if out == nil || len(msg.Result.StructuredContent) == 0 {
		return nil
	}
	return json.Unmarshal(msg.Result.StructuredContent, out)
}

// rpcPayload returns the JSON-RPC message from a JSON or SSE response.
func rpcPayload(resp *http.Response) ([]byte, error) {
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			return []byte(data), nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("no data in event stream")
}
