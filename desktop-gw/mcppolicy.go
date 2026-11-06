package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// parseToolList turns GW_MCP_TOOLS ("a, b,c") into a set. Empty means every
// tool the child advertises is allowed.
func parseToolList(s string) map[string]bool {
	var allow map[string]bool
	for _, name := range strings.Split(s, ",") {
		if name = strings.TrimSpace(name); name != "" {
			if allow == nil {
				allow = map[string]bool{}
			}
			allow[name] = true
		}
	}
	return allow
}

func toolCallName(params json.RawMessage) string {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(params, &p)
	return p.Name
}

// filterTools drops tools outside allow from a tools/list response. Every tool
// definition is sent to the model on each turn, so a short list saves context.
func filterTools(raw []byte, allow map[string]bool) ([]byte, int) {
	var resp rpcResponse
	if json.Unmarshal(raw, &resp) != nil || resp.Result == nil {
		return raw, 0
	}
	var tools []json.RawMessage
	if json.Unmarshal(resp.Result["tools"], &tools) != nil {
		return raw, 0
	}
	if allow == nil {
		return raw, len(tools)
	}
	kept := tools[:0]
	for _, t := range tools {
		var def struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t, &def) == nil && allow[def.Name] {
			kept = append(kept, t)
		}
	}
	resp.Result["tools"], _ = json.Marshal(kept)
	out, err := json.Marshal(resp)
	if err != nil {
		return raw, len(tools)
	}
	return out, len(kept)
}

// callStats describes what a tools/call result costs the model.
type callStats struct {
	images     int
	imageBytes int
	isError    bool
}

func summarizeResult(raw []byte) callStats {
	var resp struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Data string `json:"data"`
			} `json:"content"`
		} `json:"result"`
	}
	var st callStats
	if json.Unmarshal(raw, &resp) != nil {
		return st
	}
	st.isError = resp.Result.IsError || len(resp.Error) > 0
	for _, c := range resp.Result.Content {
		if c.Type == "image" {
			st.images++
			st.imageBytes += base64.StdEncoding.DecodedLen(len(c.Data))
		}
	}
	return st
}
