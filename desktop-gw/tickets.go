package main

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

type ticket struct {
	expires time.Time
	control bool
}

// ticketStore issues short-lived, single-use credentials for the screen
// WebSocket so long-lived tokens never appear in URLs or iframe content.
type ticketStore struct {
	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[string]ticket
}

func newTicketStore(ttl time.Duration) *ticketStore {
	return &ticketStore{ttl: ttl, now: time.Now, m: map[string]ticket{}}
}

func (s *ticketStore) issue(control bool) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	id := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, t := range s.m {
		if now.After(t.expires) {
			delete(s.m, k)
		}
	}
	s.m[id] = ticket{expires: now.Add(s.ttl), control: control}
	return id
}

func (s *ticketStore) redeem(id string) (ticket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[id]
	delete(s.m, id)
	if !ok || s.now().After(t.expires) {
		return ticket{}, false
	}
	return t, true
}
