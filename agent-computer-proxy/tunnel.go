package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// Tunnel opens connections to a port of a Substrate actor through
// atenet-router's CONNECT listener (Substrate "arbitrary-port ingress"). The
// router authenticates this process by its podidentity client certificate,
// resumes the actor if it is suspended, and forwards the tunnel to it.
type Tunnel struct {
	Router string // host:port of the router's CONNECT-over-TLS listener (8444)
	TLS    *tls.Config
}

// NewTunnelTLS loads the pod's podidentity certificate bundle (certificate
// chain and key in one PEM) and the trust bundle for the router's serving
// certificate. The files are reread per connection: the podCertificate
// volume rotates them.
func NewTunnelTLS(bundle, roots string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(bundle, bundle)
			return &cert, err
		},
		// The router's certificate is checked against the servicedns roots in
		// VerifyConnection; the hostname is the Service DNS name.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			pem, err := os.ReadFile(roots)
			if err != nil {
				return err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return errors.New("no certificates in router trust bundle")
			}
			if len(cs.PeerCertificates) == 0 {
				return errors.New("router sent no certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err = cs.PeerCertificates[0].Verify(x509.VerifyOptions{DNSName: cs.ServerName, Roots: pool, Intermediates: inter})
			return err
		},
	}
}

// Dial returns a connection to port on actor (`<atespace>/<name>`). The
// CONNECT authority only carries the port; the routing header selects the
// actor.
func (t *Tunnel) Dial(ctx context.Context, actor string, port int) (net.Conn, error) {
	d := tls.Dialer{Config: t.TLS.Clone(), NetDialer: &net.Dialer{Timeout: 10 * time.Second}}
	host, _, _ := net.SplitHostPort(t.Router)
	d.Config.ServerName = host
	conn, err := d.DialContext(ctx, "tcp", t.Router)
	if err != nil {
		return nil, fmt.Errorf("dial router: %w", err)
	}
	authority := fmt.Sprintf("actor-upstream:%d", port)
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nate-target-actor: %s\r\n\r\n", authority, authority, actor)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("router CONNECT: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("router CONNECT: %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 { // bytes the router sent after its 200 belong to the stream
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
