// Command desktop-gw is the single process a desktop sandbox exposes. On one
// port it serves a minimal HTML viewer, streams the X11 display over a
// WebSocket, injects viewer input through XTEST when control is granted, and
// bridges a stdio MCP server to Streamable HTTP. The bridge adds an MCP Apps
// tool (desktop_view) so MCP hosts can render the live screen inline.
package main

import (
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed html-client.html
var htmlClient []byte

//go:embed host-demo.html
var hostDemo []byte

type config struct {
	listen   string
	token    string
	display  string
	mcpCmd   string
	fps      int
	quality  int
	viewOnly bool
	tlsCert  string
	tlsKey   string
	clientCA string
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "probe":
			os.Exit(probe(os.Args[2:]))
		case "proxy":
			os.Exit(proxy(os.Args[2:]))
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("desktop-gw: %v", err)
	}
	if err := run(cfg); err != nil {
		log.Fatalf("desktop-gw: %v", err)
	}
}

func loadConfig() (config, error) {
	cfg := config{
		listen:   envOr("GW_LISTEN", "127.0.0.1:8080"),
		token:    os.Getenv("GW_TOKEN"),
		display:  envOr("DISPLAY", ":0"),
		mcpCmd:   os.Getenv("GW_MCP_CMD"),
		fps:      envInt("GW_FPS", 8),
		quality:  envInt("GW_JPEG_QUALITY", 70),
		viewOnly: os.Getenv("GW_VIEW_ONLY") == "1",
		tlsCert:  os.Getenv("GW_TLS_CERT"),
		tlsKey:   os.Getenv("GW_TLS_KEY"),
		clientCA: os.Getenv("GW_TLS_CLIENT_CA"),
	}
	if file := os.Getenv("GW_TOKEN_FILE"); file != "" && cfg.token == "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return cfg, fmt.Errorf("read GW_TOKEN_FILE: %w", err)
		}
		cfg.token = strings.TrimSpace(string(b))
	}
	if cfg.token == "" && !loopbackAddr(cfg.listen) {
		return cfg, errors.New("GW_TOKEN is required when GW_LISTEN is not a loopback address")
	}
	if (cfg.tlsCert == "") != (cfg.tlsKey == "") {
		return cfg, errors.New("GW_TLS_CERT and GW_TLS_KEY must be set together")
	}
	if cfg.clientCA != "" && cfg.tlsCert == "" {
		return cfg, errors.New("GW_TLS_CLIENT_CA requires GW_TLS_CERT and GW_TLS_KEY")
	}
	if cfg.fps < 1 || cfg.fps > 30 {
		return cfg, fmt.Errorf("GW_FPS must be 1..30, got %d", cfg.fps)
	}
	if cfg.quality < 10 || cfg.quality > 95 {
		return cfg, fmt.Errorf("GW_JPEG_QUALITY must be 10..95, got %d", cfg.quality)
	}
	return cfg, nil
}

func run(cfg config) error {
	disp, err := openDisplay(cfg.display)
	if err != nil {
		return err
	}
	defer disp.close()

	tickets := newTicketStore(60 * time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", servePage(htmlClient, "'self'"))
	// The demo host loads the view in a srcdoc iframe, which inherits this
	// policy; the view's own CSP meta (from _meta.ui.csp) narrows it again.
	mux.HandleFunc("GET /host-demo", servePage(hostDemo, "'self' ws: wss:"))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.Handle("POST /ticket", requireToken(cfg.token, ticketHandler(tickets, cfg.viewOnly)))
	mux.Handle("GET /ws/screen", newScreenHandler(disp, cfg, tickets))
	if cfg.mcpCmd != "" {
		b := newBridge(cfg.mcpCmd, 4, 30*time.Minute, 120*time.Second)
		b.app = &viewerApp{tickets: tickets, html: htmlClient, viewOnly: cfg.viewOnly, trustForwarded: cfg.clientCA != ""}
		defer b.closeAll()
		mux.Handle("/mcp", requireToken(cfg.token, b))
	}

	srv := &http.Server{Addr: cfg.listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	mode := "http"
	if cfg.tlsCert != "" {
		mode = "https"
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13}
		if cfg.clientCA != "" {
			pool, err := loadPool(cfg.clientCA)
			if err != nil {
				return err
			}
			srv.TLSConfig.ClientCAs = pool
			srv.TLSConfig.ClientAuth = tls.RequireAndVerifyClientCert
			mode = "mtls"
		}
	}
	log.Printf("desktop-gw listening on %s (%s, display %s %dx%d, mcp=%t, view-only=%t)",
		cfg.listen, mode, cfg.display, disp.width, disp.height, cfg.mcpCmd != "", cfg.viewOnly)
	if cfg.tlsCert != "" {
		return srv.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
	}
	return srv.ListenAndServe()
}

func servePage(page []byte, connectSrc string) http.HandlerFunc {
	csp := "default-src 'none'; img-src data: blob:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src " + connectSrc + "; frame-ancestors 'self'"
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		_, _ = w.Write(page)
	}
}

func requireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tokenMatches(want, bearer(r)) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func tokenMatches(want []byte, got string) bool {
	return len(want) > 0 && subtle.ConstantTimeCompare(want, []byte(got)) == 1
}

func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loadPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no PEM certificates", path)
	}
	return pool, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
	}
	return n
}
