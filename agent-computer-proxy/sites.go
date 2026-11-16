package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

// Sites is the agent computer's site policy: a directory of domain lists (a
// ConfigMap). Substrate rejects CONNECT from sandboxes and ends all TLS at its
// egress gateway, so the lists are enforced in two places instead:
//
//   - allow-*.txt become the origins of the sandbox's kagent EgressPolicy:
//     the gateway lets nothing else out.
//   - deny-*.txt become Chromium's URLBlocklist (managed policy written into
//     the sandbox): EgressPolicy has no deny rules.
//
// A domain covers itself and its subdomains ("=host": that host only). Lines
// may be in hosts-file format ("0.0.0.0 ads.example.com"), so published DNS
// blocklists can be pasted in. # starts a comment.
type Sites struct {
	Dir string
}

type siteLists struct{ Allow, Deny []string }

// Load reads the lists. Entries are sorted and de-duplicated, so two loads of
// the same files compare equal.
func (s *Sites) Load() (siteLists, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return siteLists{}, err
	}
	var l siteLists
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".txt") {
			continue
		}
		var into *[]string
		switch {
		case strings.HasPrefix(name, "allow"):
			into = &l.Allow
		case strings.HasPrefix(name, "deny"):
			into = &l.Deny
		default:
			continue
		}
		b, err := os.ReadFile(s.Dir + "/" + name)
		if err != nil {
			return siteLists{}, err
		}
		*into = append(*into, parseList(string(b))...)
	}
	slices.Sort(l.Allow)
	slices.Sort(l.Deny)
	l.Allow, l.Deny = slices.Compact(l.Allow), slices.Compact(l.Deny)
	return l, nil
}

// parseList reads domains, one per line: plain, with a scheme or "*." prefix,
// "=host", or hosts-file style ("0.0.0.0 host").
func parseList(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(strings.ToLower(line))
		if len(fields) == 0 || (len(fields) > 1 && net.ParseIP(fields[0]) == nil) {
			continue
		}
		d := fields[len(fields)-1]
		d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
		d = strings.TrimSuffix(strings.TrimPrefix(d, "*."), "/")
		host := strings.TrimPrefix(d, "=")
		if host == "" || host == "localhost" || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
			continue
		}
		out = append(out, d)
	}
	return out
}

// Origins renders the allow list as EgressPolicy origins. kagent wildcards
// cover one label, so a domain gives "https://d" and "https://*.d": deeper
// subdomains (a.b.d) need their own line.
func (l siteLists) Origins() []string {
	var out []string
	for _, d := range l.Allow {
		if host, exact := strings.CutPrefix(d, "="); exact {
			out = append(out, "https://"+host)
			continue
		}
		out = append(out, "https://"+d, "https://*."+d)
	}
	return out
}

// chromiumBlocklistMax is Chromium's URLBlocklist limit; later entries are ignored.
const chromiumBlocklistMax = 1000

// ChromiumPolicy renders the deny list as a Chromium managed policy. A bare
// host in URLBlocklist blocks it and its subdomains; ".host" blocks only it.
func (l siteLists) ChromiumPolicy() []byte {
	block := make([]string, 0, len(l.Deny))
	for _, d := range l.Deny {
		if host, exact := strings.CutPrefix(d, "="); exact {
			block = append(block, "."+host)
		} else {
			block = append(block, d)
		}
	}
	if len(block) > chromiumBlocklistMax {
		log.Printf("sites: %d deny entries, Chromium uses the first %d", len(block), chromiumBlocklistMax)
		block = block[:chromiumBlocklistMax]
	}
	b, _ := json.MarshalIndent(map[string]any{"URLBlocklist": block}, "", "  ")
	return b
}

// chromiumPolicyPath is where the sandbox file API may write (under
// /data/workspace); the image links /etc/chromium/policies/managed/
// agent-computer.json to it. Chromium reads it at start and on its policy
// refresh, so a change reaches a running browser within minutes.
const chromiumPolicyPath = "/data/workspace/chromium-policy.json"

// Kube patches one EgressPolicy through the API server with the pod's
// service account (RBAC: get and patch on that name only).
type Kube struct {
	Host      string // https://kubernetes.default.svc
	TokenFile string
	CAFile    string
}

func inCluster() *Kube {
	dir := "/var/run/secrets/kubernetes.io/serviceaccount/"
	return &Kube{Host: "https://kubernetes.default.svc", TokenFile: dir + "token", CAFile: dir + "ca.crt"}
}

// ApplyEgress server-side-applies the policy's destinations.
func (k *Kube) ApplyEgress(ctx context.Context, namespace, name string, origins []string) error {
	dests := make([]map[string]string, 0, len(origins))
	for _, o := range origins {
		dests = append(dests, map[string]string{"origin": o})
	}
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "EgressPolicy",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec":     map[string]any{"destinations": dests},
	})
	url := fmt.Sprintf("%s/apis/api.kagent.dev/v1alpha3/namespaces/%s/egresspolicies/%s?fieldManager=agent-computer-proxy&force=true", k.Host, namespace, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	token, err := os.ReadFile(k.TokenFile)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/apply-patch+yaml") // JSON is YAML
	client, err := k.client()
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("apply EgressPolicy %s: %s %s", name, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

func (k *Kube) client() (*http.Client, error) {
	if k.CAFile == "" {
		return http.DefaultClient, nil
	}
	pem, err := os.ReadFile(k.CAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, nil
}

// syncSites applies the lists whenever they change: the EgressPolicy at
// once, the Chromium policy into the sandbox if it is awake (otherwise at the
// next desktop start, see startDesktop).
func (c *Computer) syncSites(ctx context.Context) {
	var last string
	for {
		if l, err := c.Sites.Load(); err != nil {
			log.Printf("sites: %v", err)
		} else if key := fmt.Sprint(l); key != last {
			err := c.Kube.ApplyEgress(ctx, c.Namespace, c.EgressPolicy, l.Origins())
			if err != nil {
				log.Printf("sites: %v", err)
			} else {
				log.Printf("sites: %d allowed domains -> EgressPolicy %s (%d origins), %d denied -> Chromium", len(l.Allow), c.EgressPolicy, len(l.Origins()), len(l.Deny))
				c.mu.Lock()
				c.lists = l
				sandbox := ""
				if c.sb != nil && c.sb.State == stateReady {
					sandbox = c.sb.ID
				}
				c.mu.Unlock()
				if sandbox != "" {
					if err := c.writeChromiumPolicy(ctx, l, sandbox); err != nil {
						log.Printf("sites: chromium policy: %v", err)
					}
				}
				last = key
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// writeChromiumPolicy writes the deny list into the sandbox.
func (c *Computer) writeChromiumPolicy(ctx context.Context, l siteLists, sandboxID string) error {
	return c.Kagent.Call(ctx, "write_sandbox_file", map[string]any{
		"sandbox_id": sandboxID, "path": chromiumPolicyPath,
		"data_base64": base64.StdEncoding.EncodeToString(l.ChromiumPolicy()), "mode": 0o644,
	}, nil)
}
