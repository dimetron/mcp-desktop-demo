package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/jpeg"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"

	"github.com/coder/websocket"
)

// clientTLS builds an mTLS client configuration; all paths are optional.
func clientTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13}
	if caFile != "" {
		pool, err := loadPool(caFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// probe reads the hello and one frame from /ws/screen and saves the JPEG.
func probe(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	u := fs.String("url", "ws://127.0.0.1:8080/ws/screen", "screen WebSocket URL")
	token := fs.String("token", os.Getenv("GW_TOKEN"), "bearer token")
	out := fs.String("out", "", "write the first frame to this JPEG file")
	ca := fs.String("cacert", "", "CA bundle for wss://")
	cert := fs.String("cert", "", "client certificate for mTLS")
	key := fs.String("key", "", "client key for mTLS")
	timeout := fs.Duration("timeout", 15*time.Second, "overall timeout")
	if fs.Parse(args) != nil {
		return 2
	}
	tlsCfg, err := clientTLS(*ca, *cert, *key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	h := http.Header{}
	if *token != "" {
		h.Set("Authorization", "Bearer "+*token)
	}
	c, _, err := websocket.Dial(ctx, *u, &websocket.DialOptions{
		HTTPHeader: h,
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe: dial:", err)
		return 1
	}
	defer c.CloseNow()
	c.SetReadLimit(32 << 20)
	_, hello, err := c.Read(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe: hello:", err)
		return 1
	}
	var hi screenHello
	if err := json.Unmarshal(hello, &hi); err != nil {
		fmt.Fprintln(os.Stderr, "probe: bad hello:", err)
		return 1
	}
	typ, frame, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || !bytes.HasPrefix(frame, []byte{0xff, 0xd8}) {
		fmt.Fprintln(os.Stderr, "probe: expected a binary JPEG frame:", err)
		return 1
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(frame))
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe: decode:", err)
		return 1
	}
	if *out != "" {
		if err := os.WriteFile(*out, frame, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "probe:", err)
			return 1
		}
	}
	fmt.Printf("ok display=%dx%d control=%t frame=%dx%d jpeg_bytes=%d\n", hi.Width, hi.Height, hi.Control, cfg.Width, cfg.Height, len(frame))
	_ = c.Close(websocket.StatusNormalClosure, "")
	return 0
}

// proxy is a local reverse proxy that presents a client certificate to an
// mTLS gateway. It stands in for the kagent gateway leg so a browser without
// a client certificate can reach the sandbox.
func proxy(args []string) int {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:9443", "local listen address (loopback only)")
	upstream := fs.String("upstream", "", "gateway URL, e.g. https://127.0.0.1:18443")
	ca := fs.String("cacert", "", "CA bundle that signed the gateway certificate")
	cert := fs.String("cert", "", "client certificate")
	key := fs.String("key", "", "client key")
	if fs.Parse(args) != nil {
		return 2
	}
	target, err := url.Parse(*upstream)
	if err != nil || target.Scheme != "https" || !loopbackAddr(*listen) {
		fmt.Fprintln(os.Stderr, "proxy: need -upstream https://... and a loopback -listen")
		return 2
	}
	tlsCfg, err := clientTLS(*ca, *cert, *key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "proxy:", err)
		return 1
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
		},
		Transport: &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: false},
	}
	log.Printf("desktop-gw proxy %s -> %s (mTLS)", *listen, target)
	srv := &http.Server{Addr: *listen, Handler: rp, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "proxy:", err)
		return 1
	}
	return 0
}
