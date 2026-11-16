package main

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// Clipboard and secret typing for the viewer (pi-bot spec 104). They run on a
// connection of their own, because selections need an event loop and the
// capture/input connection reads no events.
//
//	{"t":"clip-paste","text":"…"}  own CLIPBOARD with text, then press Ctrl+V
//	{"t":"clip-copy","id":7}       press Ctrl+C, read CLIPBOARD, reply
//	{"t":"type-secret","text":"…"} type text key by key, never via the clipboard
//
// Replies are text frames: {"t":"clip","id":7,"text":"…"} or {"t":"clip","id":7,"error":"…"}.
// Text is never logged, only its size.

const (
	maxClipBytes   = 256 << 10
	maxSecretBytes = 1 << 10
)

// Keysyms the gateway presses itself.
const (
	ksShiftL   = 0xffe1
	ksControlL = 0xffe3
	ksReturn   = 0xff0d
	ksTab      = 0xff09
)

type clipboard struct {
	d    *display // input (XTEST) and the keymap
	conn *xgb.Conn
	win  xproto.Window

	clipAtom, targets, utf8, text, str, prop xproto.Atom

	mu    sync.Mutex
	owned string // text we serve while we own CLIPBOARD
	have  bool
	reply chan xproto.SelectionNotifyEvent

	typing sync.Mutex // one paste/copy/secret at a time
	maxReq int        // largest ChangeProperty payload the server takes
}

func openClipboard(name string, d *display) (*clipboard, error) {
	c, err := xgb.NewConnDisplay(name)
	if err != nil {
		return nil, fmt.Errorf("clipboard: connect to X display %q: %w", name, err)
	}
	setup := xproto.Setup(c)
	win, err := xproto.NewWindowId(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	scr := setup.DefaultScreen(c)
	if err := xproto.CreateWindowChecked(c, 0, win, scr.Root, -10, -10, 1, 1, 0,
		xproto.WindowClassInputOnly, 0, xproto.CwEventMask, []uint32{xproto.EventMaskPropertyChange}).Check(); err != nil {
		c.Close()
		return nil, fmt.Errorf("clipboard window: %w", err)
	}
	cb := &clipboard{d: d, conn: c, win: win, reply: make(chan xproto.SelectionNotifyEvent, 1)}
	cb.maxReq = int(setup.MaximumRequestLength)*4 - 64
	for _, a := range []struct {
		dst  *xproto.Atom
		name string
	}{{&cb.clipAtom, "CLIPBOARD"}, {&cb.targets, "TARGETS"}, {&cb.utf8, "UTF8_STRING"}, {&cb.text, "TEXT"}, {&cb.str, "STRING"}, {&cb.prop, "PIBOT_CLIP"}} {
		r, err := xproto.InternAtom(c, false, uint16(len(a.name)), a.name).Reply()
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("atom %s: %w", a.name, err)
		}
		*a.dst = r.Atom
	}
	go cb.events()
	return cb, nil
}

func (cb *clipboard) close() { cb.conn.Close() }

// events serves selection requests for text we own and hands conversion
// results to a waiting copy.
func (cb *clipboard) events() {
	for {
		ev, err := cb.conn.WaitForEvent()
		if ev == nil && err == nil {
			return // connection closed
		}
		switch e := ev.(type) {
		case xproto.SelectionRequestEvent:
			cb.serve(e)
		case xproto.SelectionClearEvent:
			cb.mu.Lock()
			cb.owned, cb.have = "", false
			cb.mu.Unlock()
		case xproto.SelectionNotifyEvent:
			select {
			case cb.reply <- e:
			default:
			}
		}
	}
}

// serve answers another client asking for our CLIPBOARD contents.
func (cb *clipboard) serve(e xproto.SelectionRequestEvent) {
	cb.mu.Lock()
	text, have := cb.owned, cb.have
	cb.mu.Unlock()
	prop := e.Property
	if prop == xproto.AtomNone { // obsolete clients
		prop = e.Target
	}
	ok := have && e.Selection == cb.clipAtom
	if ok {
		switch e.Target {
		case cb.targets:
			atoms := []xproto.Atom{cb.targets, cb.utf8, cb.text, cb.str}
			buf := make([]byte, 4*len(atoms))
			for i, a := range atoms {
				xgb.Put32(buf[4*i:], uint32(a))
			}
			xproto.ChangeProperty(cb.conn, xproto.PropModeReplace, e.Requestor, prop, xproto.AtomAtom, 32, uint32(len(atoms)), buf)
		case cb.utf8, cb.text, cb.str:
			typ := cb.utf8
			if e.Target == cb.str {
				typ = cb.str
			}
			xproto.ChangeProperty(cb.conn, xproto.PropModeReplace, e.Requestor, prop, typ, 8, uint32(len(text)), []byte(text))
		default:
			ok = false
		}
	}
	if !ok {
		prop = xproto.AtomNone
	}
	n := xproto.SelectionNotifyEvent{Time: e.Time, Requestor: e.Requestor, Selection: e.Selection, Target: e.Target, Property: prop}
	xproto.SendEvent(cb.conn, false, e.Requestor, 0, string(n.Bytes()))
}

// paste makes text the desktop clipboard and presses Ctrl+V.
func (cb *clipboard) paste(text string) error {
	if err := checkText(text, min(maxClipBytes, cb.maxReq)); err != nil {
		return err
	}
	cb.typing.Lock()
	defer cb.typing.Unlock()
	cb.mu.Lock()
	cb.owned, cb.have = text, true
	cb.mu.Unlock()
	xproto.SetSelectionOwner(cb.conn, cb.win, cb.clipAtom, xproto.TimeCurrentTime)
	owner, err := xproto.GetSelectionOwner(cb.conn, cb.clipAtom).Reply()
	if err != nil || owner.Owner != cb.win {
		return errors.New("could not take the clipboard")
	}
	cb.chord(ksControlL, 'v')
	log.Printf("clipboard: paste %d bytes", len(text))
	return nil
}

// copy presses Ctrl+C and returns what the clipboard then holds.
func (cb *clipboard) copy() (string, error) {
	cb.typing.Lock()
	defer cb.typing.Unlock()
	cb.chord(ksControlL, 'c')
	time.Sleep(250 * time.Millisecond) // let the app take the selection
	select {
	case <-cb.reply: // drop a stale notification
	default:
	}
	xproto.ConvertSelection(cb.conn, cb.win, cb.clipAtom, cb.utf8, cb.prop, xproto.TimeCurrentTime)
	var n xproto.SelectionNotifyEvent
	select {
	case n = <-cb.reply:
	case <-time.After(2 * time.Second):
		return "", errors.New("the clipboard owner didn't answer")
	}
	if n.Property == xproto.AtomNone {
		return "", errors.New("empty")
	}
	r, err := xproto.GetProperty(cb.conn, true, cb.win, cb.prop, xproto.GetPropertyTypeAny, 0, maxClipBytes/4+1).Reply()
	if err != nil {
		return "", err
	}
	if r.BytesAfter > 0 || r.Format != 8 {
		return "", errors.New("the copied text is too large or not text")
	}
	text := string(r.Value)
	if text == "" {
		return "", errors.New("empty")
	}
	log.Printf("clipboard: copy %d bytes", len(text))
	return text, nil
}

// typeSecret types text into the focused window. Characters the keymap lacks
// are typed through a spare keycode remapped for the moment, like xdotool.
func (cb *clipboard) typeSecret(text string) error {
	if err := checkText(text, maxSecretBytes); err != nil {
		return err
	}
	cb.typing.Lock()
	defer cb.typing.Unlock()
	plan := typePlan(text, cb.d.keymap)
	spare := cb.d.spareKeycode()
	for _, k := range plan {
		code, shift := k.code, k.shift
		if k.remap != 0 {
			if spare == 0 {
				return errors.New("no spare keycode for a character outside the keyboard layout")
			}
			cb.d.remap(spare, k.remap)
			code, shift = spare, false
			time.Sleep(20 * time.Millisecond) // clients reload the mapping on MappingNotify
		}
		if shift {
			cb.d.key(cb.d.keymap[ksShiftL].code, true)
		}
		cb.d.key(code, true)
		cb.d.key(code, false)
		if shift {
			cb.d.key(cb.d.keymap[ksShiftL].code, false)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if spare != 0 {
		cb.d.remap(spare, 0)
	}
	log.Printf("clipboard: typed a secret, %d chars", len(plan))
	return nil
}

// chord presses mod+key (both keysyms) and releases them.
func (cb *clipboard) chord(mod, key uint32) {
	m, k := cb.d.keymap[mod].code, cb.d.keymap[key].code
	cb.d.key(m, true)
	cb.d.key(k, true)
	cb.d.key(k, false)
	cb.d.key(m, false)
}

func checkText(text string, limit int) error {
	switch {
	case text == "":
		return errors.New("empty")
	case len(text) > limit:
		return fmt.Errorf("text is %d bytes, the limit is %d", len(text), limit)
	case !utf8.ValidString(text):
		return errors.New("text is not UTF-8")
	}
	return nil
}

// keyStroke is one character to type: a keycode (Shift for the second
// column) or, when the layout lacks it, a keysym to remap a spare key to.
type keyStroke struct {
	code  xproto.Keycode
	shift bool
	remap uint32
}

// typePlan maps text to keystrokes against the current keymap.
func typePlan(text string, keymap map[uint32]keyBinding) []keyStroke {
	var out []keyStroke
	for _, r := range text {
		if r == '\r' {
			continue // \r\n is one Return
		}
		ks := runeKeysym(r)
		if b, ok := keymap[ks]; ok && b.column <= 1 {
			out = append(out, keyStroke{code: b.code, shift: b.column == 1})
			continue
		}
		out = append(out, keyStroke{remap: ks})
	}
	return out
}

// runeKeysym is the X11 keysym for a character: Latin-1 by code point, other
// Unicode as 0x01000000|cp, newline and tab as their keys.
func runeKeysym(r rune) uint32 {
	switch {
	case r == '\n':
		return ksReturn
	case r == '\t':
		return ksTab
	case r < 0x100:
		return uint32(r)
	}
	return 0x01000000 | uint32(r)
}
