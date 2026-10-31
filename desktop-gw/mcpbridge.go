package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	headerSession  = "Mcp-Session-Id"
	maxMessageSize = 16 << 20
)

// rpcMessage is the subset of a JSON-RPC 2.0 message the bridge routes on.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

func (m rpcMessage) isRequest() bool { return m.Method != "" && len(m.ID) > 0 }

func idKey(id json.RawMessage) string { return string(bytes.TrimSpace(id)) }

func rpcError(id json.RawMessage, code int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
	return b
}

func rpcResult(id json.RawMessage, result any) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return b
}

// bridge exposes a stdio MCP server as Streamable HTTP (JSON response mode).
// Each MCP session owns one child process, so sessions never share state.
type bridge struct {
	cmd         string
	maxSessions int
	idle        time.Duration
	timeout     time.Duration
	app         *viewerApp

	mu       sync.Mutex
	sessions map[string]*session
	stop     chan struct{}
}

func newBridge(cmd string, maxSessions int, idle, timeout time.Duration) *bridge {
	b := &bridge{cmd: cmd, maxSessions: maxSessions, idle: idle, timeout: timeout,
		sessions: map[string]*session{}, stop: make(chan struct{})}
	go b.reap()
	return b
}

func (b *bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		b.post(w, r)
	case http.MethodDelete:
		if s := b.lookup(r.Header.Get(headerSession)); s != nil {
			b.remove(s)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "unknown session", http.StatusNotFound)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "server-initiated streams are not supported", http.StatusMethodNotAllowed)
	}
}

func (b *bridge) post(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMessageSize+1))
	if err != nil || len(body) > maxMessageSize {
		http.Error(w, "request too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		http.Error(w, "JSON-RPC batches are not supported", http.StatusBadRequest)
		return
	}
	var msg rpcMessage
	if err := json.Unmarshal(body, &msg); err != nil || msg.JSONRPC != "2.0" {
		http.Error(w, "invalid JSON-RPC message", http.StatusBadRequest)
		return
	}

	sid := r.Header.Get(headerSession)
	var s *session
	switch {
	case msg.Method == "initialize" && sid == "":
		if s, err = b.create(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		s.uiHost = hostSupportsApps(msg.Params)
	case sid == "":
		http.Error(w, "missing "+headerSession, http.StatusBadRequest)
		return
	default:
		if s = b.lookup(sid); s == nil {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
	}
	s.touch()
	w.Header().Set(headerSession, s.id)

	if !msg.isRequest() {
		if err := s.send(body); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if b.app != nil && s.uiHost {
		if local, ok := b.app.handleLocal(r, msg); ok {
			writeJSON(w, local)
			return
		}
	}
	resp, err := s.call(r, msg.ID, body, b.timeout)
	if err != nil {
		writeJSON(w, rpcError(msg.ID, -32603, err.Error()))
		return
	}
	if b.app != nil && s.uiHost {
		resp = b.app.augment(msg, resp)
	}
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (b *bridge) create() (*session, error) {
	b.mu.Lock()
	n := len(b.sessions)
	b.mu.Unlock()
	if n >= b.maxSessions {
		return nil, fmt.Errorf("session limit %d reached", b.maxSessions)
	}
	s, err := startSession(b.cmd)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.sessions[s.id] = s
	b.mu.Unlock()
	go func() {
		<-s.dead
		b.mu.Lock()
		delete(b.sessions, s.id)
		b.mu.Unlock()
	}()
	return s, nil
}

func (b *bridge) lookup(id string) *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[id]
}

func (b *bridge) remove(s *session) {
	b.mu.Lock()
	delete(b.sessions, s.id)
	b.mu.Unlock()
	s.kill()
}

func (b *bridge) reap() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
		}
		b.mu.Lock()
		var stale []*session
		for _, s := range b.sessions {
			if s.idleFor() > b.idle {
				stale = append(stale, s)
			}
		}
		b.mu.Unlock()
		for _, s := range stale {
			log.Printf("mcp: closing idle session %s", s.id)
			b.remove(s)
		}
	}
}

func (b *bridge) closeAll() {
	close(b.stop)
	b.mu.Lock()
	all := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		all = append(all, s)
	}
	b.mu.Unlock()
	for _, s := range all {
		b.remove(s)
	}
}

// hostSupportsApps reports whether the MCP client advertised the MCP Apps
// extension (io.modelcontextprotocol/ui) with the HTML app MIME type.
func hostSupportsApps(params json.RawMessage) bool {
	var p struct {
		Capabilities struct {
			Extensions map[string]struct {
				MimeTypes []string `json:"mimeTypes"`
			} `json:"extensions"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(params, &p) != nil {
		return false
	}
	for _, m := range p.Capabilities.Extensions[appExtension].MimeTypes {
		if m == appMimeType {
			return true
		}
	}
	return false
}

// session is one stdio MCP child process and its in-flight requests.
type session struct {
	id     string
	uiHost bool
	cmd    *exec.Cmd
	dead   chan struct{}

	wmu   sync.Mutex
	stdin io.WriteCloser

	mu       sync.Mutex
	pending  map[string]chan []byte
	lastUsed time.Time
}

func startSession(command string) (*session, error) {
	idb := make([]byte, 16)
	_, _ = rand.Read(idb)
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = &prefixWriter{prefix: "mcp-child: ", w: os.Stderr}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start MCP server: %w", err)
	}
	s := &session{id: hex.EncodeToString(idb), cmd: cmd, stdin: stdin, dead: make(chan struct{}),
		pending: map[string]chan []byte{}, lastUsed: time.Now()}
	go s.readLoop(stdout)
	return s, nil
}

func (s *session) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *session) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) > 0 {
		return 0
	}
	return time.Since(s.lastUsed)
}

func (s *session) send(line []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	select {
	case <-s.dead:
		return fmt.Errorf("MCP server exited")
	default:
	}
	if bytes.ContainsAny(line, "\n\r") {
		var compact bytes.Buffer
		if err := json.Compact(&compact, line); err != nil {
			return err
		}
		line = compact.Bytes()
	}
	_, err := s.stdin.Write(append(line, '\n'))
	return err
}

func (s *session) call(r *http.Request, id json.RawMessage, body []byte, timeout time.Duration) ([]byte, error) {
	key := idKey(id)
	ch := make(chan []byte, 1)
	s.mu.Lock()
	if _, dup := s.pending[key]; dup {
		s.mu.Unlock()
		return nil, fmt.Errorf("request id %s already in flight", key)
	}
	s.pending[key] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, key)
		s.lastUsed = time.Now()
		s.mu.Unlock()
	}()
	if err := s.send(body); err != nil {
		return nil, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case resp := <-ch:
		return resp, nil
	case <-s.dead:
		return nil, fmt.Errorf("MCP server exited")
	case <-t.C:
		return nil, fmt.Errorf("MCP server did not answer within %s", timeout)
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
}

// readLoop routes child output: responses go to their waiting HTTP request,
// server notifications are dropped (no SSE stream), and server-to-client
// requests are refused because the HTTP client cannot receive them.
func (s *session) readLoop(stdout io.Reader) {
	defer func() {
		_ = s.cmd.Wait()
		close(s.dead)
	}()
	rd := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, err := rd.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			s.route(line)
		}
		if err != nil {
			return
		}
	}
}

func (s *session) route(line []byte) {
	var msg rpcMessage
	if json.Unmarshal(line, &msg) != nil {
		log.Printf("mcp: dropping non-JSON output line (%d bytes)", len(line))
		return
	}
	switch {
	case msg.isRequest():
		_ = s.send(rpcError(msg.ID, -32601, "client does not accept server requests"))
	case msg.Method != "":
	default:
		s.mu.Lock()
		ch := s.pending[idKey(msg.ID)]
		s.mu.Unlock()
		if ch != nil {
			select {
			case ch <- line:
			default:
				log.Printf("mcp: dropping duplicate response for id %s", idKey(msg.ID))
			}
		}
	}
}

func (s *session) kill() {
	if s.cmd.Process != nil {
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	}
	_ = s.stdin.Close()
	select {
	case <-s.dead:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	}
}

type prefixWriter struct {
	prefix string
	w      io.Writer
	mu     sync.Mutex
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		fmt.Fprintf(p.w, "%s%s\n", p.prefix, p.buf[:i])
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}
