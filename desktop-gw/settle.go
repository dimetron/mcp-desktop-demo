package main

import (
	"time"
)

// settleLimits are the tools after which the gateway waits for the screen to
// stop changing before it answers, with the longest wait for each. Tools such
// as browser_navigate return once the navigation starts, so an agent that
// looks right away sees a blank or half-drawn page. Waiting here means its
// next screenshot or state read shows the loaded page.
var settleLimits = map[string]time.Duration{
	"browser_navigate": 10 * time.Second,
	"page":             10 * time.Second,
	"launch_app":       10 * time.Second,
	"browser_click":    5 * time.Second,
	"browser_pointer":  4 * time.Second,
	"browser_select":   3 * time.Second,
	"browser_type":     2 * time.Second,
	"click":            5 * time.Second,
	"double_click":     5 * time.Second,
	"right_click":      3 * time.Second,
	"press_key":        5 * time.Second,
	"hotkey":           5 * time.Second,
	"type_text":        2 * time.Second,
	"scroll":           2 * time.Second,
	"drag":             3 * time.Second,
	"set_value":        2 * time.Second,
	"invoke_menu":      3 * time.Second,
}

const (
	settleInterval = 200 * time.Millisecond
	// settleQuiet is how long the screen must stay still. A page that stops
	// painting for this long has, in practice, finished loading.
	settleQuiet = 800 * time.Millisecond
	// settleTolerance is the share of sampled pixels that may still change:
	// spinners, carets and animated widgets never stop, and they must not
	// hold every call for the full limit.
	settleTolerance = 0.004
	settleStride    = 16 // sample every 16th pixel
)

// screenSampler returns a sparse sample of the screen.
type screenSampler func() ([]uint32, error)

func displaySampler(d *display) screenSampler {
	return func() ([]uint32, error) {
		raw, err := d.grab()
		if err != nil {
			return nil, err
		}
		n := len(raw) / 4 / settleStride
		out := make([]uint32, n)
		for i := range out {
			o := i * settleStride * 4
			out[i] = uint32(raw[o]) | uint32(raw[o+1])<<8 | uint32(raw[o+2])<<16
		}
		return out, nil
	}
}

// changed is the share of samples that differ.
func changed(a, b []uint32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 1
	}
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return float64(n) / float64(len(a))
}

// waitSettled polls the screen until it has been (nearly) still for
// settleQuiet, or limit passes. It returns how long it waited and whether the
// screen settled.
func waitSettled(sample screenSampler, limit time.Duration, sleep func(time.Duration)) (time.Duration, bool) {
	prev, err := sample()
	if err != nil {
		return 0, false
	}
	var waited, still time.Duration
	for waited < limit {
		sleep(settleInterval)
		waited += settleInterval
		cur, err := sample()
		if err != nil {
			return waited, false
		}
		if changed(prev, cur) <= settleTolerance {
			still += settleInterval
			if still >= settleQuiet {
				return waited, true
			}
		} else {
			still = 0
		}
		prev = cur
	}
	return waited, false
}
