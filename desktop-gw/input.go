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

	// Clipboard messages (clipboard.go).
	Text string `json:"text,omitempty"`
	ID   int    `json:"id,omitempty"`
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

// key presses or releases one keycode.
func (d *display) key(code xproto.Keycode, down bool) {
	typ := byte(xKeyRelease)
	if down {
		typ = xKeyPress
	}
	xtest.FakeInput(d.conn, typ, byte(code), 0, d.root, 0, 0, 0)
}

// spareKeycode is the highest keycode with no keysyms, or 0.
func (d *display) spareKeycode() xproto.Keycode {
	if d.perCode <= 0 {
		return 0
	}
	for i := len(d.syms)/d.perCode - 1; i >= 0; i-- {
		empty := true
		for _, s := range d.syms[i*d.perCode : (i+1)*d.perCode] {
			if s != 0 {
				empty = false
				break
			}
		}
		if empty {
			return d.minCode + xproto.Keycode(i)
		}
	}
	return 0
}

// remap points code at keysym ks in its first two columns (0 clears it) and
// waits until the server has applied it.
func (d *display) remap(code xproto.Keycode, ks uint32) {
	syms := make([]xproto.Keysym, d.perCode)
	if ks != 0 {
		syms[0] = xproto.Keysym(ks)
		if d.perCode > 1 {
			syms[1] = xproto.Keysym(ks)
		}
	}
	xproto.ChangeKeyboardMapping(d.conn, 1, code, byte(d.perCode), syms)
	xproto.GetInputFocus(d.conn).Reply() // round trip: the change is in
}

func (d *display) inject(ev inputEvent) {
	for _, f := range translateInput(ev, d.width, d.height, d.keymap) {
		xtest.FakeInput(d.conn, f.typ, f.detail, 0, d.root, f.x, f.y, 0)
	}
}
