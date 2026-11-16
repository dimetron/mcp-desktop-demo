package main

import (
	"encoding/json"
	"testing"
	"time"
)

// frames returns a sampler that plays the given screens in order, then
// repeats the last one.
func frames(screens ...[]uint32) screenSampler {
	i := 0
	return func() ([]uint32, error) {
		f := screens[min(i, len(screens)-1)]
		i++
		return f, nil
	}
}

func screen(v uint32, changedSamples int) []uint32 {
	s := make([]uint32, 1000)
	for i := range changedSamples {
		s[i] = v
	}
	return s
}

func noSleep(time.Duration) {}

func TestWaitSettledAfterLoading(t *testing.T) {
	// Blank, then three loading frames that differ a lot, then still.
	waited, ok := waitSettled(frames(screen(0, 0), screen(1, 500), screen(2, 500), screen(3, 500)), 10*time.Second, noSleep)
	if !ok {
		t.Fatal("did not settle")
	}
	// 3 changing intervals, then settleQuiet of stillness.
	if want := 3*settleInterval + settleQuiet; waited != want {
		t.Fatalf("waited %s, want %s", waited, want)
	}
}

func TestWaitSettledToleratesSmallAnimation(t *testing.T) {
	// 2 of 1000 samples (a spinner) keep changing: below the tolerance.
	alt := []([]uint32){screen(1, 2), screen(2, 2)}
	i := 0
	sample := func() ([]uint32, error) { i++; return alt[i%2], nil }
	if _, ok := waitSettled(sample, 5*time.Second, noSleep); !ok {
		t.Fatal("a small animation must not block settling")
	}
}

func TestWaitSettledGivesUpAtLimit(t *testing.T) {
	i := uint32(0)
	sample := func() ([]uint32, error) { i++; return screen(i, 900), nil }
	waited, ok := waitSettled(sample, 2*time.Second, noSleep)
	if ok || waited != 2*time.Second {
		t.Fatalf("waited %s settled %t, want the 2s limit", waited, ok)
	}
}

func TestToolCallArgs(t *testing.T) {
	params := json.RawMessage(`{"name":"click","arguments":{"session":"shots","x":1}}`)
	if got := toolCallArg(params, "session"); got != "shots" {
		t.Fatalf("session = %q", got)
	}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"start_session","arguments":{"session":"shots"}}}`)
	var m struct {
		Params struct {
			Arguments map[string]string `json:"arguments"`
		} `json:"params"`
	}
	json.Unmarshal(withDefaultArg(body, "cursor_theme", "plus"), &m)
	if m.Params.Arguments["cursor_theme"] != "plus" || m.Params.Arguments["session"] != "shots" {
		t.Fatalf("args = %v", m.Params.Arguments)
	}
	set := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"start_session","arguments":{"cursor_theme":"mine"}}}`)
	json.Unmarshal(withDefaultArg(set, "cursor_theme", "plus"), &m)
	if m.Params.Arguments["cursor_theme"] != "mine" {
		t.Fatal("a caller's own theme must be kept")
	}
}
