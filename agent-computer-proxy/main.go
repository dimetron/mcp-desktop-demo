// Command agent-computer-proxy runs the agent computer (mcp-desktop) in an
// Agent Substrate sandbox and serves it on one HTTP port, like the old
// Deployment did: agents' MCP at /mcp, the screen viewer at /.
//
// The sandbox is a kagent sandbox (SandboxTemplate mcp-desktop). kagent can't
// forward sandbox ports (ADR gap 1), so the proxy reaches desktop-gw on port
// 8080 through Substrate's atenet-router (arbitrary-port ingress), which
// wakes a suspended actor on request. The proxy also owns the lifecycle:
// it creates the sandbox, recreates it before its TTL ends, starts the
// desktop after every wake (sandbox snapshots keep files, not processes),
// and suspends it when idle.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	stateReady     = "RUNTIME_STATE_READY"
	stateSuspended = "RUNTIME_STATE_SUSPENDED"
	desktopPort    = 8080
)

// Computer is one long-lived agent computer.
type Computer struct {
	Kagent    *Kagent
	Tunnel    *Tunnel
	Namespace string
	Template  string
	Name      string
	TTL       time.Duration
	Idle      time.Duration
	Token     func() (string, error)
	// Site policy (sites.go): lists -> EgressPolicy and Chromium URLBlocklist.
	Sites        *Sites
	Kube         *Kube
	EgressPolicy string
	lists        siteLists

	mu           sync.Mutex
	sb           *Sandbox
	healthyUntil time.Time
	failures     int      // failed starts in a row
	replaced     []string // sandboxes deleted as broken; never picked again
	// Sign-in handoff (handoff.go): each desktop's cookie export token,
	// and the sandbox that aged out, whose cookies go to the next one.
	exportTokens map[string]string
	handoff      *Sandbox

	lastActive atomic.Int64 // unix seconds
	inflight   atomic.Int64
}

func (c *Computer) actor() string { return c.Namespace + "/session-" + c.sb.ID }

// usable reports whether a sandbox can keep serving for a while.
func usable(sb *Sandbox, now time.Time) bool {
	if sb.Failure != nil {
		return false
	}
	switch sb.State {
	case stateReady, stateSuspended, "RUNTIME_STATE_CREATING":
	default:
		return false
	}
	return sb.ExpiresAt.IsZero() || sb.ExpiresAt.After(now.Add(10*time.Minute))
}

// current returns the sandbox to use, finding or creating it. Call with mu held.
func (c *Computer) current(ctx context.Context) (*Sandbox, error) {
	now := time.Now()
	if c.sb != nil {
		var sb Sandbox
		if err := c.Kagent.Call(ctx, "get_sandbox", map[string]any{"sandbox_id": c.sb.ID}, &sb); err == nil && usable(&sb, now) {
			c.sb = &sb
			return c.sb, nil
		} else if err == nil && sb.Failure != nil {
			_ = c.replace(ctx, fmt.Errorf("%s: %s", sb.Failure.Reason, sb.Failure.Message))
		} else if err == nil {
			c.handoff = &sb // near its TTL: carry its sign-ins to the next one
		}
		c.sb = nil
	}
	var list struct {
		Sandboxes []Sandbox `json:"sandboxes"`
	}
	if err := c.Kagent.Call(ctx, "list_sandboxes", map[string]any{"page_size": 100}, &list); err != nil {
		return nil, err
	}
	var best *Sandbox
	for i := range list.Sandboxes {
		sb := &list.Sandboxes[i]
		if sb.Name == c.Name && sb.Template.Name == c.Template && usable(sb, now) && !slices.Contains(c.replaced, sb.ID) && (best == nil || sb.ExpiresAt.After(best.ExpiresAt)) {
			best = sb
		}
	}
	if best == nil {
		var sb Sandbox
		err := c.Kagent.Call(ctx, "create_sandbox", map[string]any{
			"namespace": c.Namespace, "template": c.Template, "name": c.Name,
			"request_id": fmt.Sprintf("%s-%d", c.Name, now.Unix()), "ttl_seconds": int(c.TTL.Seconds()),
		}, &sb)
		if err != nil {
			return nil, err
		}
		log.Printf("created sandbox %s (expires %s)", sb.ID, sb.ExpiresAt.Format(time.RFC3339))
		best = &sb
	}
	c.sb = best
	if err := c.waitState(ctx, stateReady, 3*time.Minute); err != nil {
		return nil, c.replace(ctx, err)
	}
	return c.sb, nil
}

// waitState polls until the sandbox reaches state. Call with mu held.
func (c *Computer) waitState(ctx context.Context, state string, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		var sb Sandbox
		if err := c.Kagent.Call(ctx, "get_sandbox", map[string]any{"sandbox_id": c.sb.ID}, &sb); err == nil {
			c.sb = &sb
			if sb.State == state && sb.Operation == "RUNTIME_OPERATION_NONE" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sandbox %s not %s: %w", c.sb.ID, state, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// Ensure makes the desktop answer and returns the actor to route to.
func (c *Computer) Ensure(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sb != nil && time.Now().Before(c.healthyUntil) {
		return c.actor(), nil
	}
	sb, err := c.current(ctx)
	if err != nil {
		return "", err
	}
	if sb.State == stateSuspended {
		if err := c.Kagent.Call(ctx, "resume_sandbox", map[string]any{"sandbox_id": sb.ID}, nil); err != nil {
			return "", err
		}
		if err := c.waitState(ctx, stateReady, 2*time.Minute); err != nil {
			return "", err
		}
		log.Printf("woke sandbox %s", c.sb.ID)
	}
	if !c.healthy(ctx) {
		if err := c.startDesktop(ctx); err != nil {
			return "", c.failed(ctx, err)
		}
		deadline := time.Now().Add(90 * time.Second)
		for !c.healthy(ctx) {
			if time.Now().After(deadline) {
				return "", c.failed(ctx, errors.New("desktop did not become healthy"))
			}
			time.Sleep(time.Second)
		}
		log.Printf("desktop up in sandbox %s", c.sb.ID)
	}
	if old := c.handoff; old != nil && old.ID != c.sb.ID {
		c.handoff = nil
		c.handOff(old)
	}
	c.failures = 0
	c.healthyUntil = time.Now().Add(20 * time.Second)
	return c.actor(), nil
}

// maxStartFailures is how many failed starts in a row make the sandbox count
// as broken. Substrate kills a sandbox over its memory limit and marks the
// actor CRASHED, while kagent keeps reporting it READY; nothing resumes it.
const maxStartFailures = 3

// failed counts a failed start and, past the limit, deletes the sandbox so
// the next request creates a fresh one. Call with mu held.
func (c *Computer) failed(ctx context.Context, err error) error {
	c.failures++
	if c.failures < maxStartFailures || c.sb == nil {
		return err
	}
	return c.replace(ctx, err)
}

// replace deletes the current sandbox as broken; the next request creates a
// fresh one. Call with mu held.
func (c *Computer) replace(ctx context.Context, err error) error {
	if c.sb == nil {
		return err
	}
	id := c.sb.ID
	log.Printf("sandbox %s is broken (%v): replacing it", id, err)
	if derr := c.Kagent.Call(ctx, "delete_sandbox", map[string]any{"sandbox_id": id}, nil); derr != nil {
		log.Printf("delete sandbox %s: %v", id, derr)
	}
	c.sb, c.failures, c.healthyUntil = nil, 0, time.Time{}
	c.replaced = append(c.replaced, id)
	return err
}

// healthy checks desktop-gw's /healthz through the router. Call with mu held.
func (c *Computer) healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://actor-upstream:8080/healthz", nil)
	resp, err := c.client(c.actor()).Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 300
}

// startDesktop runs start-desktop.sh in the sandbox. The token travels in the
// process environment (the template's env holds only literal values).
func (c *Computer) startDesktop(ctx context.Context) error {
	token, err := c.Token()
	if err != nil {
		return err
	}
	// Chromium reads its managed policy at start: write the deny list first.
	if err := c.writeChromiumPolicy(ctx, c.lists, c.sb.ID); err != nil {
		log.Printf("sites: chromium policy: %v", err)
	}
	return c.Kagent.Call(ctx, "start_sandbox_process", map[string]any{
		"sandbox_id": c.sb.ID,
		"command":    []string{"start-desktop.sh"},
		"env": map[string]string{"GW_TOKEN": token, "GW_LISTEN": fmt.Sprintf("0.0.0.0:%d", desktopPort),
			"GW_COOKIE_EXPORT_TOKEN": c.exportTokenFor(c.sb.ID)},
	}, nil)
}

func (c *Computer) transport(actor string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return c.Tunnel.Dial(ctx, actor, desktopPort)
		},
		// The router may move the actor between requests: never reuse a tunnel.
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 2 * time.Minute,
	}
}

func (c *Computer) client(actor string) *http.Client {
	return &http.Client{Transport: c.transport(actor)}
}

// Sleep suspends the sandbox. Substrate keeps its files; the desktop restarts on wake.
func (c *Computer) Sleep(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sb == nil {
		if _, err := c.current(ctx); err != nil {
			return err
		}
	}
	if c.sb.State != stateReady {
		return nil
	}
	if err := c.Kagent.Call(ctx, "suspend_sandbox", map[string]any{"sandbox_id": c.sb.ID}, nil); err != nil {
		return err
	}
	c.healthyUntil = time.Time{}
	log.Printf("suspended sandbox %s", c.sb.ID)
	return c.waitState(ctx, stateSuspended, 2*time.Minute)
}

// Status reports the sandbox without waking it.
type Status struct {
	State     string    `json:"state"` // awake, asleep, starting, missing
	SandboxID string    `json:"sandboxId,omitempty"`
	Raw       string    `json:"runtimeState,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
	IdleSleep string    `json:"idleSleep"`
}

func (c *Computer) Status(ctx context.Context) (Status, error) {
	st := Status{State: "missing", IdleSleep: c.Idle.String()}
	// A wake can hold the lock for minutes; report it instead of waiting.
	if !c.mu.TryLock() {
		st.State = "starting"
		return st, nil
	}
	defer c.mu.Unlock()
	sb := c.sb
	if sb == nil {
		var list struct {
			Sandboxes []Sandbox `json:"sandboxes"`
		}
		if err := c.Kagent.Call(ctx, "list_sandboxes", map[string]any{"page_size": 100}, &list); err != nil {
			return st, err
		}
		for i := range list.Sandboxes {
			if s := &list.Sandboxes[i]; s.Name == c.Name && s.Template.Name == c.Template && usable(s, time.Now()) && !slices.Contains(c.replaced, s.ID) {
				sb = s
			}
		}
		if sb == nil {
			return st, nil
		}
	} else {
		var fresh Sandbox
		if err := c.Kagent.Call(ctx, "get_sandbox", map[string]any{"sandbox_id": sb.ID}, &fresh); err == nil {
			sb, c.sb = &fresh, &fresh
		}
	}
	st.SandboxID, st.Raw, st.ExpiresAt = sb.ID, sb.State, sb.ExpiresAt
	switch {
	case sb.Failure != nil:
		st.State = "failed" // the next request replaces it
	case sb.Operation != "" && sb.Operation != "RUNTIME_OPERATION_NONE":
		st.State = "starting"
	case sb.State == stateReady:
		st.State = "awake"
	case sb.State == stateSuspended:
		st.State = "asleep"
	default:
		st.State = "starting"
	}
	return st, nil
}

// idleSleeper suspends the sandbox when nothing has used it for c.Idle and
// no request (a viewer's screen stream, say) is still open.
func (c *Computer) idleSleeper(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		idle := time.Since(time.Unix(c.lastActive.Load(), 0))
		if c.inflight.Load() == 0 && idle > c.Idle {
			if err := c.Sleep(ctx); err != nil {
				log.Printf("idle sleep: %v", err)
			}
		}
	}
}

func (c *Computer) authorized(r *http.Request) bool {
	want, err := c.Token()
	if err != nil || want == "" {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (c *Computer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/livez": // the proxy itself, for Kubernetes probes
		w.WriteHeader(http.StatusNoContent)
		return
	case "/computer/status", "/computer/wake", "/computer/sleep":
		c.control(w, r)
		return
	case exportPath: // the proxy's own handoff route, never from outside
		http.NotFound(w, r)
		return
	}
	c.inflight.Add(1)
	defer c.inflight.Add(-1)
	log.Printf("request %s %s from %s (%s)", r.Method, r.URL.Path, r.RemoteAddr, r.UserAgent())
	c.lastActive.Store(time.Now().Unix())
	defer func() { c.lastActive.Store(time.Now().Unix()) }()
	actor, err := c.Ensure(r.Context())
	if err != nil {
		log.Printf("ensure: %v", err)
		http.Error(w, "agent computer is starting or unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: fmt.Sprintf("actor-upstream:%d", desktopPort)})
			pr.Out.Host = pr.In.Host // desktop-gw checks Origin against Host for the screen WebSocket
		},
		Transport:     c.transport(actor),
		FlushInterval: -1, // MCP responses stream as SSE
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			c.mu.Lock()
			c.healthyUntil = time.Time{} // re-check the desktop on the next request
			c.mu.Unlock()
			http.Error(w, "agent computer: "+err.Error(), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// control serves status, wake and sleep for PiBot, with the desktop's token.
func (c *Computer) control(w http.ResponseWriter, r *http.Request) {
	if !c.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path != "/computer/status" && r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	var err error
	switch r.URL.Path {
	case "/computer/wake":
		c.lastActive.Store(time.Now().Unix())
		_, err = c.Ensure(ctx)
	case "/computer/sleep":
		err = c.Sleep(ctx)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	st, err := c.Status(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	listen := flag.String("listen", env("LISTEN", ":8080"), "listen address")
	idle := flag.Duration("idle", 15*time.Minute, "suspend the sandbox after this long without use")
	ttl := flag.Duration("ttl", 24*time.Hour, "sandbox lifetime (kagent caps it; the proxy recreates it before it ends)")
	flag.Parse()

	tokenFile := env("TOKEN_FILE", "/var/run/desktop-gw/token")
	c := &Computer{
		Kagent:       &Kagent{URL: env("KAGENT_MCP_URL", "http://kagent-controller.kagent.svc.cluster.local:8083/mcp"), HTTP: &http.Client{Timeout: 2 * time.Minute}},
		Tunnel:       &Tunnel{Router: env("ROUTER_ADDR", "atenet-router.ate-system.svc:8444"), TLS: NewTunnelTLS(env("IDENTITY_BUNDLE", "/run/podidentity/credential-bundle.pem"), env("ROUTER_TRUST", "/run/servicedns/trust-bundle.pem"))},
		Namespace:    env("SANDBOX_NAMESPACE", "kagent"),
		Template:     env("SANDBOX_TEMPLATE", "mcp-desktop"),
		Name:         env("SANDBOX_NAME", "agent-computer"),
		Sites:        &Sites{Dir: env("SITE_LISTS", "/etc/agent-computer/sites")},
		Kube:         inCluster(),
		EgressPolicy: env("SITE_EGRESS_POLICY", "agent-computer-web-egress"),
		TTL:          *ttl,
		Idle:         *idle,
		Token: func() (string, error) {
			b, err := os.ReadFile(tokenFile)
			return strings.TrimSpace(string(b)), err
		},
	}
	c.lastActive.Store(time.Now().Unix())
	go c.idleSleeper(context.Background())
	log.Printf("agent-computer-proxy on %s: sandbox %s/%s (template %s), idle sleep %s", *listen, c.Namespace, c.Name, c.Template, c.Idle)
	go c.syncSites(context.Background())
	srv := &http.Server{Addr: *listen, Handler: c, ReadHeaderTimeout: 15 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
