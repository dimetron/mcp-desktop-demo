package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	appExtension = "io.modelcontextprotocol/ui"
	appMimeType  = "text/html;profile=mcp-app"
	viewerURI    = "ui://desktop-gw/viewer"
	viewerTool   = "desktop_view"
	// ticketMetaKey carries the WebSocket URL and ticket in CallToolResult._meta,
	// which hosts forward to the view but do not add to the model context.
	ticketMetaKey = "dev.kagent/desktop"
)

// viewerApp adds an MCP Apps view (SEP-1865) of the live desktop to any
// bridged MCP server: one ui:// resource and one tool that opens it.
type viewerApp struct {
	tickets        *ticketStore
	html           []byte
	viewOnly       bool
	trustForwarded bool
}

func (a *viewerApp) toolDef() json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"name":  viewerTool,
		"title": "Show live desktop",
		"description": "Show the user a live view of this sandbox desktop. " +
			"Set control=true when the user must use mouse and keyboard (for example to sign in).",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"control": map[string]any{"type": "boolean", "description": "Allow the user to send input."},
			},
			"additionalProperties": false,
		},
		"_meta": map[string]any{"ui": map[string]any{"resourceUri": viewerURI, "visibility": []string{"model", "app"}}},
	})
	return b
}

func (a *viewerApp) resourceDef() json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"uri": viewerURI, "name": "desktop-viewer", "title": "Live desktop", "mimeType": appMimeType,
	})
	return b
}

// wsOrigin is the origin the view must connect to, as seen by the user's
// browser. Forwarded headers are trusted only when every caller is an
// mTLS-authenticated proxy.
func (a *viewerApp) wsOrigin(r *http.Request) string {
	scheme, host := "ws", r.Host
	if r.TLS != nil {
		scheme = "wss"
	}
	if a.trustForwarded {
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			host = h
			scheme = "ws"
			if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
				scheme = "wss"
			}
		}
	}
	return scheme + "://" + host
}

// handleLocal answers the requests the bridge owns instead of the child.
func (a *viewerApp) handleLocal(r *http.Request, msg rpcMessage) ([]byte, bool) {
	switch msg.Method {
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		if json.Unmarshal(msg.Params, &p) != nil || p.URI != viewerURI {
			return nil, false
		}
		return rpcResult(msg.ID, map[string]any{"contents": []any{map[string]any{
			"uri": viewerURI, "mimeType": appMimeType, "text": string(a.html),
			"_meta": map[string]any{"ui": map[string]any{
				"csp":           map[string]any{"connectDomains": []string{a.wsOrigin(r)}},
				"prefersBorder": true,
			}},
		}}}), true
	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				Control bool `json:"control"`
			} `json:"arguments"`
		}
		if json.Unmarshal(msg.Params, &p) != nil || p.Name != viewerTool {
			return nil, false
		}
		control := p.Arguments.Control && !a.viewOnly
		mode := "view only"
		if control {
			mode = "user control enabled"
		}
		return rpcResult(msg.ID, map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": "Showing the live desktop to the user (" + mode + ")."}},
			"structuredContent": map[string]any{"control": control},
			"_meta": map[string]any{ticketMetaKey: map[string]any{
				"wsUrl":  a.wsOrigin(r) + "/ws/screen",
				"ticket": a.tickets.issue(control),
			}},
		}), true
	}
	return nil, false
}

type rpcResponse struct {
	JSONRPC string                     `json:"jsonrpc"`
	ID      json.RawMessage            `json:"id"`
	Result  map[string]json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage            `json:"error,omitempty"`
}

// augment adds the viewer to child responses: the resources capability, the
// first tools/list page, and the first resources/list page (synthesized when
// the child has no resources support).
func (a *viewerApp) augment(req rpcMessage, raw []byte) []byte {
	var resp rpcResponse
	if json.Unmarshal(raw, &resp) != nil {
		return raw
	}
	var page struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(req.Params, &page)
	firstPage := page.Cursor == ""
	switch req.Method {
	case "initialize":
		if resp.Result == nil {
			return raw
		}
		var caps map[string]json.RawMessage
		_ = json.Unmarshal(resp.Result["capabilities"], &caps)
		if caps == nil {
			caps = map[string]json.RawMessage{}
		}
		if _, ok := caps["resources"]; !ok {
			caps["resources"] = json.RawMessage(`{}`)
		}
		resp.Result["capabilities"], _ = json.Marshal(caps)
	case "tools/list":
		if resp.Result == nil || !firstPage {
			return raw
		}
		resp.Result["tools"] = appendRaw(resp.Result["tools"], a.toolDef())
	case "resources/list":
		if !firstPage {
			return raw
		}
		if resp.Result == nil {
			resp.Error = nil
			resp.Result = map[string]json.RawMessage{}
		}
		resp.Result["resources"] = appendRaw(resp.Result["resources"], a.resourceDef())
	default:
		return raw
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return raw
	}
	return out
}

func appendRaw(list json.RawMessage, item json.RawMessage) json.RawMessage {
	var items []json.RawMessage
	_ = json.Unmarshal(list, &items)
	out, _ := json.Marshal(append(items, item))
	return out
}

// ticketHandler gives the standalone viewer a single-use screen ticket in
// exchange for the bearer token checked by requireToken.
func ticketHandler(tickets *ticketStore, viewOnly bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, []byte(fmt.Sprintf(`{"ticket":%q,"control":%t}`, tickets.issue(!viewOnly), !viewOnly)))
	}
}
