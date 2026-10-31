package main

import (
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// XTEST FakeInput event types (core X protocol event codes).
const (
	xKeyPress      = 2
	xKeyRelease    = 3
	xButtonPress   = 4
	xButtonRelease = 5
	xMotionNotify  = 6
)

// inputEvent is one viewer input message:
//
//	{"t":"move","x":10,"y":20}
//	{"t":"button","b":1,"down":true,"x":10,"y":20}
//	{"t":"wheel","dy":-1}
//	{"t":"key","keysym":65293,"down":true}
//
// Modifiers are sent as their own key events; the gateway never synthesizes them.
type inputEvent struct {
	T      string `json:"t"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	B      int    `json:"b"`
	Down   bool   `json:"down"`
	DY     int    `json:"dy"`
	Keysym uint32 `json:"keysym"`
}

type keyBinding struct {
	code   xproto.Keycode
	column int
}

// buildKeymap maps each keysym to the keycode that produces it in the lowest
// column (column 0 = unshifted, 1 = shifted). The viewer sends Shift itself,
// so the shifted keysym resolves to the same physical key as its base symbol.
func buildKeymap(first xproto.Keycode, perCode int, syms []xproto.Keysym) map[uint32]keyBinding {
	m := map[uint32]keyBinding{}
	if perCode <= 0 {
		return m
	}
	for i, s := range syms {
		if s == 0 {
			continue
		}
		b := keyBinding{code: first + xproto.Keycode(i/perCode), column: i % perCode}
		if old, ok := m[uint32(s)]; !ok || b.column < old.column {
			m[uint32(s)] = b
		}
	}
	return m
}

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}

// fakeCall is one XTEST FakeInput request.
type fakeCall struct {
	typ, detail byte
	x, y        int16
}

// translateInput turns a viewer event into XTEST calls. Invalid or unmapped
// events produce no calls.
func translateInput(ev inputEvent, w, h int, keymap map[uint32]keyBinding) []fakeCall {
	x, y := int16(clamp(ev.X, 0, w-1)), int16(clamp(ev.Y, 0, h-1))
	switch ev.T {
	case "move":
		return []fakeCall{{xMotionNotify, 0, x, y}}
	case "button":
		if ev.B < 1 || ev.B > 3 {
			return nil
		}
		typ := byte(xButtonRelease)
		if ev.Down {
			typ = xButtonPress
		}
		return []fakeCall{{xMotionNotify, 0, x, y}, {typ, byte(ev.B), x, y}}
	case "wheel":
		btn := byte(5)
		if ev.DY < 0 {
			btn = 4
		} else if ev.DY == 0 {
			return nil
		}
		return []fakeCall{{xButtonPress, btn, 0, 0}, {xButtonRelease, btn, 0, 0}}
	case "key":
		k, ok := keymap[ev.Keysym]
		if !ok {
			return nil
		}
		typ := byte(xKeyRelease)
		if ev.Down {
			typ = xKeyPress
		}
		return []fakeCall{{typ, byte(k.code), 0, 0}}
	}
	return nil
}

func (d *display) inject(ev inputEvent) {
	for _, f := range translateInput(ev, d.width, d.height, d.keymap) {
		xtest.FakeInput(d.conn, f.typ, f.detail, 0, d.root, f.x, f.y, 0)
	}
}
