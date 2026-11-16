package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jezek/xgb/xproto"
)

func TestTypePlan(t *testing.T) {
	// US-like map: 'a' unshifted on 38, 'A' and '!' shifted, Return on 36.
	keymap := map[uint32]keyBinding{
		'a': {code: 38, column: 0}, 'A': {code: 38, column: 1},
		'1': {code: 10, column: 0}, '!': {code: 10, column: 1},
		ksReturn: {code: 36, column: 0},
		'é':      {code: 50, column: 2}, // AltGr column: not typed directly
	}
	got := typePlan("aA!\r\néї", keymap)
	want := []keyStroke{
		{code: 38}, {code: 38, shift: true}, {code: 10, shift: true},
		{code: 36}, // \r\n is one Return
		{remap: 'é'}, {remap: 0x01000457}, // Ukrainian ї
	}
	if len(got) != len(want) {
		t.Fatalf("plan = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stroke %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCheckText(t *testing.T) {
	if checkText("", 10) == nil || checkText(strings.Repeat("x", 11), 10) == nil || checkText("\xff", 10) == nil {
		t.Fatal("empty, oversize and invalid UTF-8 must fail")
	}
	if err := checkText("привіт", 64); err != nil {
		t.Fatal(err)
	}
}

func TestHandleClipRepliesAndNeverEchoesSecrets(t *testing.T) {
	if handleClip(nil, inputEvent{T: "key", Keysym: 'a'}) != nil {
		t.Fatal("ordinary input must pass through")
	}
	var r clipReply
	b := handleClip(nil, inputEvent{T: "type-secret", Text: "hunter2"})
	if err := json.Unmarshal(b, &r); err != nil || r.T != "clip" || r.Op != "type-secret" || r.Error == "" {
		t.Fatalf("reply = %s", b)
	}
	if strings.Contains(string(b), "hunter2") {
		t.Fatalf("secret echoed: %s", b)
	}
	b = handleClip(nil, inputEvent{T: "clip-copy", ID: 7})
	if json.Unmarshal(b, &r) != nil || r.ID != 7 || r.Op != "clip-copy" {
		t.Fatalf("copy reply = %s", b)
	}
}

func TestSpareKeycodeAndRunes(t *testing.T) {
	d := &display{minCode: 8, perCode: 2, syms: []xproto.Keysym{'a', 'A', 0, 0, 'b', 'B', 0, 0}}
	if got := d.spareKeycode(); got != 11 {
		t.Fatalf("spare = %d, want 11 (highest empty)", got)
	}
	if runeKeysym('\t') != ksTab || runeKeysym('ä') != 0xe4 || runeKeysym('🙂') != 0x0101f642 {
		t.Fatal("keysyms")
	}
}
