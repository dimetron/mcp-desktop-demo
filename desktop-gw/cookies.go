package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Sign-ins for PiBot (pi-bot spec 108): PiBot posts cookies it read from the
// user's Mac Chrome, and the gateway sets them in this desktop's Chromium over
// the loopback DevTools endpoint (start-desktop.sh: 127.0.0.1:9222).
//
//	POST   /cookies               {"cookies":[CDP Network.CookieParam…]} → {"set":n,"failed":[{"domain","reason"}]}
//	GET    /cookies/domains       {"<domain>": count}; values never leave
//	DELETE /cookies?domain=x.com  every cookie of x.com and its subdomains → {"deleted":n}
//	GET    /cookies/export        all cookies, for agent-computer-proxy's sandbox handoff only;
//	                              needs GW_COOKIE_EXPORT_TOKEN, never the viewer token
//
// Cookie values are never logged: the log has the method, counts and status.

const (
	maxCookieBody     = 2 << 20
	maxCookiesPerPost = 5000
)

// cdpClient runs one DevTools command at a time on the browser target.
type cdpClient struct {
	base string // http://127.0.0.1:9222
	http *http.Client
	mu   sync.Mutex
	id   atomic.Int64
}

func newCDP(base string) *cdpClient {
	return &cdpClient{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 10 * time.Second}}
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return fmt.Sprintf("cdp %d: %s", e.Code, e.Message) }

// call sends method with params to the browser target and decodes the result.
func (c *cdpClient) call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/json/version", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("browser not reachable: %w", err)
	}
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v)
	resp.Body.Close()
	if err != nil || v.WS == "" {
		return errors.New("browser not reachable: no DevTools websocket")
	}
	conn, _, err := websocket.Dial(ctx, v.WS, nil)
	if err != nil {
		return fmt.Errorf("browser DevTools: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 20) // getCookies on a full jar
	id := c.id.Add(1)
	msg, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		return err
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var r struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *cdpError       `json:"error"`
		}
		if json.Unmarshal(data, &r) != nil || r.ID != id {
			continue // an event
		}
		conn.Close(websocket.StatusNormalClosure, "")
		if r.Error != nil {
			return r.Error
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(r.Result, result)
	}
}

// cookie is CDP's Network.Cookie / CookieParam, kept as raw JSON fields so
// whatever PiBot or Chromium sends goes through unchanged.
type cookie map[string]any

func (c cookie) domain() string { s, _ := c["domain"].(string); return s }

// matchesSite: x.com matches x.com, .x.com and any subdomain.
func matchesSite(domain, site string) bool {
	d := strings.ToLower(strings.TrimPrefix(domain, "."))
	site = strings.ToLower(strings.TrimPrefix(site, "."))
	return site != "" && (d == site || strings.HasSuffix(d, "."+site))
}

type cookieAPI struct {
	cdp *cdpClient
}

func (a *cookieAPI) getAll(ctx context.Context) ([]cookie, error) {
	var r struct {
		Cookies []cookie `json:"cookies"`
	}
	err := a.cdp.call(ctx, "Storage.getCookies", map[string]any{}, &r)
	return r.Cookies, err
}

// set sets cookies in one call; if Chromium rejects the batch, it retries
// one by one so a bad cookie fails only itself.
func (a *cookieAPI) set(ctx context.Context, cs []cookie) (int, map[string]string, error) {
	failed := map[string]string{}
	if len(cs) == 0 {
		return 0, failed, nil
	}
	err := a.cdp.call(ctx, "Storage.setCookies", map[string]any{"cookies": cs}, nil)
	var ce *cdpError
	if err == nil {
		return len(cs), failed, nil
	}
	if !errors.As(err, &ce) {
		return 0, nil, err
	}
	n := 0
	for _, c := range cs {
		if err := a.cdp.call(ctx, "Storage.setCookies", map[string]any{"cookies": []cookie{c}}, nil); err != nil {
			if !errors.As(err, &ce) {
				return n, nil, err
			}
			failed[c.domain()] = ce.Message
			continue
		}
		n++
	}
	return n, failed, nil
}

// paramOf turns a Network.Cookie from getCookies back into a CookieParam.
func paramOf(c cookie) cookie {
	p := cookie{}
	for _, k := range []string{"name", "value", "domain", "path", "secure", "httpOnly", "sameSite", "priority", "sourceScheme", "sourcePort", "partitionKey"} {
		if v, ok := c[k]; ok {
			p[k] = v
		}
	}
	if s, ok := c["session"].(bool); !ok || !s {
		if e, ok := c["expires"].(float64); ok && e > 0 {
			p["expires"] = e
		}
	}
	return p
}

func (a *cookieAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	status, n := a.serve(ctx, w, r)
	log.Printf("cookies: %s %s → %d (%d)", r.Method, r.URL.Path, status, n)
}

// serve answers one request and returns the status and a count for the log.
func (a *cookieAPI) serve(ctx context.Context, w http.ResponseWriter, r *http.Request) (int, int) {
	fail := func(status int, err error) (int, int) {
		http.Error(w, err.Error(), status)
		return status, 0
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/cookies":
		var body struct {
			Cookies []cookie `json:"cookies"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCookieBody)).Decode(&body); err != nil {
			return fail(http.StatusBadRequest, errors.New("body must be {\"cookies\":[…]} under 2 MiB"))
		}
		if len(body.Cookies) > maxCookiesPerPost {
			return fail(http.StatusRequestEntityTooLarge, fmt.Errorf("at most %d cookies per request", maxCookiesPerPost))
		}
		for _, c := range body.Cookies {
			if c.domain() == "" {
				return fail(http.StatusBadRequest, errors.New("every cookie needs a domain"))
			}
		}
		n, failed, err := a.set(ctx, body.Cookies)
		if err != nil {
			return fail(http.StatusBadGateway, err)
		}
		type failure struct {
			Domain string `json:"domain"`
			Reason string `json:"reason"`
		}
		out := struct {
			Set    int       `json:"set"`
			Failed []failure `json:"failed"`
		}{Set: n, Failed: []failure{}}
		for d, why := range failed {
			out.Failed = append(out.Failed, failure{d, why})
		}
		cookieJSON(w, out)
		return http.StatusOK, n
	case r.Method == http.MethodGet && r.URL.Path == "/cookies/domains":
		all, err := a.getAll(ctx)
		if err != nil {
			return fail(http.StatusBadGateway, err)
		}
		out := map[string]int{}
		for _, c := range all {
			out[c.domain()]++
		}
		cookieJSON(w, out)
		return http.StatusOK, len(out)
	case r.Method == http.MethodDelete && r.URL.Path == "/cookies":
		site := r.URL.Query().Get("domain")
		if site == "" || strings.ContainsAny(site, "/?#: ") {
			return fail(http.StatusBadRequest, errors.New("give ?domain=example.com"))
		}
		all, err := a.getAll(ctx)
		if err != nil {
			return fail(http.StatusBadGateway, err)
		}
		// Setting a cookie with an expiry in the past deletes it; Storage
		// has no per-cookie delete on the browser target.
		var gone []cookie
		for _, c := range all {
			if matchesSite(c.domain(), site) {
				p := paramOf(c)
				p["expires"] = 1
				gone = append(gone, p)
			}
		}
		n, _, err := a.set(ctx, gone)
		if err != nil {
			return fail(http.StatusBadGateway, err)
		}
		cookieJSON(w, map[string]int{"deleted": n})
		return http.StatusOK, n
	case r.Method == http.MethodGet && r.URL.Path == "/cookies/export":
		all, err := a.getAll(ctx)
		if err != nil {
			return fail(http.StatusBadGateway, err)
		}
		out := make([]cookie, 0, len(all))
		for _, c := range all {
			out = append(out, paramOf(c))
		}
		cookieJSON(w, map[string]any{"cookies": out})
		return http.StatusOK, len(out)
	}
	return fail(http.StatusNotFound, errors.New("not found"))
}

func cookieJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// requireExportToken guards /cookies/export with its own token, which only
// agent-computer-proxy has; without one configured the route is off.
func requireExportToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(bearer(r))) != 1 {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mountCookies adds the sign-in routes to mux.
func mountCookies(mux *http.ServeMux, cfg config) {
	api := &cookieAPI{cdp: newCDP(cfg.cdpURL)}
	mux.Handle("POST /cookies", requireToken(cfg.token, api))
	mux.Handle("GET /cookies/domains", requireToken(cfg.token, api))
	mux.Handle("DELETE /cookies", requireToken(cfg.token, api))
	mux.Handle("GET /cookies/export", requireExportToken(cfg.exportToken, api))
}
