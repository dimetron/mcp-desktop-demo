package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// display is one X11 connection used for both capture (GetImage) and input
// injection (XTEST). xgb connections are safe for concurrent use.
type display struct {
	conn          *xgb.Conn
	root          xproto.Window
	width, height int
	keymap        map[uint32]keyBinding
}

func openDisplay(name string) (*display, error) {
	c, err := xgb.NewConnDisplay(name)
	if err != nil {
		return nil, fmt.Errorf("connect to X display %q: %w", name, err)
	}
	if err := xtest.Init(c); err != nil {
		c.Close()
		return nil, fmt.Errorf("XTEST extension: %w", err)
	}
	setup := xproto.Setup(c)
	if setup.ImageByteOrder != xproto.ImageOrderLSBFirst {
		c.Close()
		return nil, errors.New("only LSB-first X image byte order is supported")
	}
	scr := setup.DefaultScreen(c)
	if !hasZPixmap32(setup.PixmapFormats, scr.RootDepth) {
		c.Close()
		return nil, fmt.Errorf("root depth %d is not stored as 32 bits per pixel", scr.RootDepth)
	}
	count := int(setup.MaxKeycode) - int(setup.MinKeycode) + 1
	km, err := xproto.GetKeyboardMapping(c, setup.MinKeycode, byte(count)).Reply()
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("keyboard mapping: %w", err)
	}
	return &display{
		conn:   c,
		root:   scr.Root,
		width:  int(scr.WidthInPixels),
		height: int(scr.HeightInPixels),
		keymap: buildKeymap(setup.MinKeycode, int(km.KeysymsPerKeycode), km.Keysyms),
	}, nil
}

func hasZPixmap32(formats []xproto.Format, depth byte) bool {
	for _, f := range formats {
		if f.Depth == depth && f.BitsPerPixel == 32 {
			return true
		}
	}
	return false
}

func (d *display) close() { d.conn.Close() }

// grab returns the raw BGRX pixels of the whole root window.
func (d *display) grab() ([]byte, error) {
	r, err := xproto.GetImage(d.conn, xproto.ImageFormatZPixmap, xproto.Drawable(d.root),
		0, 0, uint16(d.width), uint16(d.height), 0xffffffff).Reply()
	if err != nil {
		return nil, err
	}
	if len(r.Data) < d.width*d.height*4 {
		return nil, fmt.Errorf("short X image: %d bytes", len(r.Data))
	}
	return r.Data, nil
}

// bgrxToRGBA converts an X11 ZPixmap (32 bpp, LSB first) into RGBA.
func bgrxToRGBA(dst *image.RGBA, src []byte) {
	p := dst.Pix
	for i := 0; i+3 < len(p) && i+3 < len(src); i += 4 {
		p[i], p[i+1], p[i+2], p[i+3] = src[i+2], src[i+1], src[i], 0xff
	}
}

// frameSource produces JPEG frames, returning changed=false when the screen
// is identical to the previous capture.
type frameSource func() (frame []byte, changed bool, err error)

func jpegSource(d *display, quality int) frameSource {
	var prev []byte
	img := image.NewRGBA(image.Rect(0, 0, d.width, d.height))
	var buf bytes.Buffer
	return func() ([]byte, bool, error) {
		raw, err := d.grab()
		if err != nil {
			return nil, false, err
		}
		if bytes.Equal(raw, prev) {
			return nil, false, nil
		}
		prev = raw
		bgrxToRGBA(img, raw)
		buf.Reset()
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, false, err
		}
		return bytes.Clone(buf.Bytes()), true, nil
	}
}

// broadcaster runs the capture loop only while at least one viewer is
// subscribed. Slow viewers lose intermediate frames instead of queueing them.
type broadcaster struct {
	src      frameSource
	interval time.Duration

	mu      sync.Mutex
	subs    map[chan []byte]struct{}
	last    []byte
	running bool
}

func newBroadcaster(src frameSource, interval time.Duration) *broadcaster {
	return &broadcaster{src: src, interval: interval, subs: map[chan []byte]struct{}{}}
}

func (b *broadcaster) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 1)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	if b.last != nil {
		ch <- b.last
	}
	if !b.running {
		b.running = true
		go b.loop()
	}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

func (b *broadcaster) loop() {
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		frame, changed, err := b.src()
		if err != nil {
			log.Printf("capture: %v", err)
		}
		b.mu.Lock()
		// Keep last across restarts: the source only reports changes, so a
		// static screen would otherwise never reach a reconnecting viewer.
		if changed {
			b.last = frame
			for ch := range b.subs {
				offerLatest(ch, frame)
			}
		}
		if len(b.subs) == 0 {
			b.running = false
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()
		<-t.C
	}
}

// offerLatest replaces any undelivered frame with the newest one.
func offerLatest(ch chan []byte, frame []byte) {
	select {
	case ch <- frame:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- frame:
	default:
	}
}

type screenHello struct {
	Width   int  `json:"w"`
	Height  int  `json:"h"`
	Control bool `json:"control"`
}

// newScreenHandler serves /ws/screen. Browsers authenticate with a
// single-use ?ticket= (WebSocket APIs cannot set headers); other clients may
// send the bearer token. The origin check is disabled because no ambient
// credential (cookie) authorizes the socket, and MCP App views run in a
// sandboxed iframe whose origin is opaque.
func newScreenHandler(d *display, cfg config, tickets *ticketStore) http.Handler {
	b := newBroadcaster(jpegSource(d, cfg.quality), time.Second/time.Duration(cfg.fps))
	want := []byte(cfg.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		control := !cfg.viewOnly
		switch {
		case r.URL.Query().Has("ticket"):
			t, ok := tickets.redeem(r.URL.Query().Get("ticket"))
			if !ok {
				http.Error(w, "invalid or expired ticket", http.StatusUnauthorized)
				return
			}
			control = control && t.control
		case len(want) == 0 || tokenMatches(want, bearer(r)):
		default:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(4096)
		streamScreen(r.Context(), c, d, b, control)
	})
}

func streamScreen(ctx context.Context, c *websocket.Conn, d *display, b *broadcaster, control bool) {
	hello, _ := json.Marshal(screenHello{Width: d.width, Height: d.height, Control: control})
	if err := writeWithTimeout(ctx, c, websocket.MessageText, hello); err != nil {
		return
	}
	if control {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		go func() {
			defer cancel()
			for {
				_, msg, err := c.Read(ctx)
				if err != nil {
					return
				}
				var ev inputEvent
				if json.Unmarshal(msg, &ev) == nil {
					d.inject(ev)
				}
			}
		}()
	} else {
		ctx = c.CloseRead(ctx)
	}
	frames, unsubscribe := b.subscribe()
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-frames:
			if err := writeWithTimeout(ctx, c, websocket.MessageBinary, f); err != nil {
				return
			}
		}
	}
}

func writeWithTimeout(ctx context.Context, c *websocket.Conn, typ websocket.MessageType, p []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.Write(ctx, typ, p)
}
