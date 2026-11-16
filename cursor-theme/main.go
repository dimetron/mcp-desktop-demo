// Command cursor-theme writes the "plus" agent cursor: a small crosshair whose
// centre is the hotspot, as a dotLottie source for `cua-driver cursor-theme
// build` (format: cua-driver docs/cursor-themes.md, profile v2).
//
//	go run . plus.lottie
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"os"
)

const (
	themeID = "dev.pibot.cursor.plus"
	canvas  = 128 // the overlay draws 128 units as about 42 points
	centre  = canvas / 2
	arm     = 30 // half-length of each bar: about 10 pt on screen
	bar     = 6  // bar thickness: about 2 pt
	edge    = 3  // white outline around the red cross
	frames  = 12 // 0.4 s at 30 fps
)

var actions = []string{"idle", "observe", "click", "drag", "scroll", "text", "key", "navigate", "app", "transfer", "record", "system"}

// rect is a Lottie rectangle shape centred on the canvas.
func rect(w, h float64) map[string]any {
	return map[string]any{"ty": "rc", "nm": "bar", "d": 1,
		"p": static([]float64{centre, centre}), "s": static([]float64{w, h}), "r": static(1.0)}
}

func static(v any) map[string]any { return map[string]any{"a": 0, "k": v} }

func fill(r, g, b float64) map[string]any {
	return map[string]any{"ty": "fl", "nm": "fill", "c": static([]float64{r, g, b, 1}), "o": static(100.0), "r": 1}
}

// layer is one shape layer: a horizontal and a vertical bar in one colour.
// Profile v2 animates layer transforms only, so the click pulse is the
// layer's scale; anchor and position at the centre keep the hotspot still.
func layer(name string, ind int, half, thick float64, r, g, b float64, scale any) map[string]any {
	return map[string]any{
		"ddd": 0, "ind": ind, "ty": 4, "nm": name, "sr": 1, "ip": 0, "op": frames, "st": 0, "bm": 0,
		"ks": map[string]any{
			"o": static(100.0), "r": static(0.0),
			"p": static([]float64{centre, centre, 0}), "a": static([]float64{centre, centre, 0}),
			"s": scale,
		},
		"shapes": []any{rect(2*half, thick), rect(thick, 2*half), fill(r, g, b)},
	}
}

// pulse grows the cross by a quarter and back over the clip (click).
func pulse() map[string]any {
	ease := map[string]any{"x": []float64{0.4}, "y": []float64{1}}
	easeOut := map[string]any{"x": []float64{0.6}, "y": []float64{0}}
	return map[string]any{"a": 1, "k": []any{
		map[string]any{"t": 0, "s": []float64{100, 100, 100}, "i": ease, "o": easeOut},
		map[string]any{"t": frames / 2, "s": []float64{125, 125, 100}, "i": ease, "o": easeOut},
		map[string]any{"t": frames, "s": []float64{100, 100, 100}},
	}}
}

func animation(action string) map[string]any {
	scale := any(static([]float64{100, 100, 100}))
	if action == "click" {
		scale = pulse()
	}
	// Layers draw top first: red cross over a slightly larger white one.
	return map[string]any{
		"v": "5.7.0", "fr": 30, "ip": 0, "op": frames, "w": canvas, "h": canvas, "nm": "action_" + action, "ddd": 0,
		"assets": []any{},
		"layers": []any{
			layer("cross", 1, arm, bar, 0.90, 0.16, 0.16, scale),
			layer("outline", 2, arm+edge, bar+2*edge, 1, 1, 1, scale),
		},
	}
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: cursor-theme <out.lottie>")
		os.Exit(2)
	}
	out, err := os.Create(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer out.Close()
	z := zip.NewWriter(out)
	write := func(name string, v any) {
		w, err := z.Create(name)
		if err != nil {
			panic(err)
		}
		if err := json.NewEncoder(w).Encode(v); err != nil {
			panic(err)
		}
	}
	var anims []any
	acts := map[string]any{}
	for _, a := range actions {
		id := "action_" + a
		write("a/"+id+".json", animation(a))
		anims = append(anims, map[string]any{"id": id})
		acts[a] = map[string]any{"animation": id, "still_frame": 0}
	}
	write("manifest.json", map[string]any{"animations": anims})
	write("cua/theme.json", map[string]any{
		"schema": "cua.cursor-theme/2", "id": themeID, "name": "Plus", "version": "1.0.0",
		"author": "pi-bot", "license": "MIT",
		"compatibility": map[string]any{"profile": "cua-driver-actions-v2", "semantics": 2},
		"canvas":        map[string]any{"width": canvas, "height": canvas, "fps": 30},
		"hotspot":       map[string]any{"x": centre, "y": centre},
		"actions":       acts,
	})
	if err := z.Close(); err != nil {
		panic(err)
	}
}
