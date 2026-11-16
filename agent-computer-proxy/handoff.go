package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// Sign-in handoff (pi-bot spec 108). When the sandbox nears its TTL, the
// proxy creates a new one; before the old one goes, its Chromium cookies are
// copied into the new desktop, so PiBot's sign-ins survive the replacement.
// desktop-gw serves GET /cookies/export only with a per-sandbox export token
// the proxy generates at start and keeps in memory; ServeHTTP never forwards
// that route from outside. If the proxy restarts, the tokens are gone and the
// handoff is skipped: PiBot's auto-sync or a manual sync restores sign-ins.

const exportPath = "/cookies/export"

// desktop-gw's POST /cookies limits.
const (
	handoffBatch = 2000
	handoffBytes = 1536 << 10
)

func newExportToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// exportTokenFor returns the export token for a sandbox, making one when its
// desktop is about to start. Call with mu held.
func (c *Computer) exportTokenFor(id string) string {
	if c.exportTokens == nil {
		c.exportTokens = map[string]string{}
	}
	if c.exportTokens[id] == "" {
		c.exportTokens[id] = newExportToken()
	}
	return c.exportTokens[id]
}

// handOff copies the old sandbox's cookies into the current one and deletes
// the old sandbox. It swaps c.sb to drive the old one (resume, start its
// desktop) with the usual helpers. Call with mu held, after the current
// desktop is healthy.
func (c *Computer) handOff(old *Sandbox) {
	token := c.exportTokens[old.ID]
	if token == "" {
		log.Printf("handoff: no export token for sandbox %s (started by an earlier proxy): sign-ins not carried", old.ID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cur := c.sb
	defer func() { c.sb = cur }()

	c.sb = old
	var sb Sandbox
	if err := c.Kagent.Call(ctx, "get_sandbox", map[string]any{"sandbox_id": old.ID}, &sb); err != nil {
		log.Printf("handoff: sandbox %s: %v", old.ID, err)
		return
	}
	c.sb = &sb
	if sb.State == stateSuspended {
		if err := c.Kagent.Call(ctx, "resume_sandbox", map[string]any{"sandbox_id": sb.ID}, nil); err != nil {
			log.Printf("handoff: resume %s: %v", sb.ID, err)
			return
		}
		if err := c.waitState(ctx, stateReady, 2*time.Minute); err != nil {
			log.Printf("handoff: %v", err)
			return
		}
	}
	// A resumed sandbox runs no desktop; Chromium's profile is still on /data.
	if !c.healthy(ctx) {
		if err := c.startDesktop(ctx); err != nil {
			log.Printf("handoff: start old desktop: %v", err)
			return
		}
		for deadline := time.Now().Add(90 * time.Second); !c.healthy(ctx); time.Sleep(time.Second) {
			if time.Now().After(deadline) {
				log.Printf("handoff: old desktop %s did not become healthy", sb.ID)
				return
			}
		}
	}
	viewer, err := c.Token()
	if err != nil {
		log.Printf("handoff: token: %v", err)
		return
	}
	n, err := carryCookies(ctx, c.client(c.actor()), token, c.client(c.Namespace+"/session-"+cur.ID), viewer)
	if err != nil {
		log.Printf("handoff: %s → %s: %v", sb.ID, cur.ID, err)
		return
	}
	log.Printf("handoff: carried %d cookies from sandbox %s to %s", n, sb.ID, cur.ID)
	if err := c.Kagent.Call(ctx, "delete_sandbox", map[string]any{"sandbox_id": sb.ID}, nil); err != nil {
		log.Printf("handoff: delete %s: %v", sb.ID, err)
	}
	delete(c.exportTokens, sb.ID)
	c.replaced = append(c.replaced, sb.ID)
}

// carryCookies exports every cookie from one desktop-gw and sets them in
// another, in batches. The cookies stay in memory only.
func carryCookies(ctx context.Context, from *http.Client, exportToken string, to *http.Client, viewerToken string) (int, error) {
	var jar struct {
		Cookies []json.RawMessage `json:"cookies"`
	}
	if err := gwCall(ctx, from, http.MethodGet, exportPath, exportToken, nil, &jar); err != nil {
		return 0, fmt.Errorf("export: %w", err)
	}
	set := 0
	for start := 0; start < len(jar.Cookies); {
		end, size := start, 0
		for end < len(jar.Cookies) && end-start < handoffBatch && (end == start || size+len(jar.Cookies[end]) <= handoffBytes) {
			size += len(jar.Cookies[end])
			end++
		}
		var reply struct {
			Set int `json:"set"`
		}
		if err := gwCall(ctx, to, http.MethodPost, "/cookies", viewerToken, map[string]any{"cookies": jar.Cookies[start:end]}, &reply); err != nil {
			return set, fmt.Errorf("import: %w", err)
		}
		set += reply.Set
		start = end
	}
	return set, nil
}

func gwCall(ctx context.Context, client *http.Client, method, path, token string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://actor-upstream:%d%s", desktopPort, path), rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errors.New("desktop-gw has no sign-in routes (older image)")
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("%s %s", resp.Status, bytes.TrimSpace(b))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
