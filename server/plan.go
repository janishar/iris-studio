package server

import (
	_ "image/jpeg" // references can be; a take never is
	"math"
	"path/filepath"
)

// aspectRatios are the sizes the ratio tiles stand for. Every side is on the
// grid and under maxDimension.
var aspectRatios = map[string][2]int{
	"1:1":  {defaultDimension, defaultDimension},
	"4:3":  {1184, 880},
	"3:4":  {880, 1184},
	"3:2":  {1248, 832},
	"2:3":  {832, 1248},
	"16:9": {1360, 768},
	"9:16": {768, 1360},
}

// iris.c, FLUX3_REF_AREA_SINGLE and FLUX3_REF_AREA_MULTI.
const (
	flux3RefAreaSingle = 4 * 1024 * 1024
	flux3RefAreaMulti  = 1024 * 1024
)

// sizePlan is what size a render comes out. A side is 0 when the engine decides
// it and that cannot be said from here. Sent is whether iris is told the size:
// without it, iris takes the size from the reference.
type sizePlan struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Source string `json:"source"`
	Sent   bool   `json:"sent"`
}

// planSize is the one place a render's size is decided. A typed side overrides
// that side of the ratio, the reference or the default.
//
// iris takes both sides or neither: main.c drops a lone -W once a reference is
// given. So one typed side sends both, and a side that cannot be worked out
// sends nothing, which validate refuses.
func planSize(cfg *Config, p map[string]any) sizePlan {
	w, h := intFrom(p["width"], 0), intFrom(p["height"], 0)
	typed := w > 0 || h > 0
	refs, _ := p["refs"].([]any)
	plan := sizePlan{Width: w, Height: h, Source: "width × height", Sent: true}
	base, chosen := aspectRatios[anyToString(p["ratio"])]
	switch {
	case w > 0 && h > 0:
		return plan
	case chosen:
		plan.Source = "chosen"
	case len(refs) > 0:
		plan.Source = "follows the first image"
		first, _ := refs[0].(map[string]any)
		session := firstString(anyToString(p["session_name"]), cfg.CurrentSession())
		path := filepath.Join(cfg.Sessions, safeStem(session), "inputs", filepath.Base(anyToString(first["name"])))
		// A file that will not decode, WebP among them, has no size to state.
		// iris may still read it, or refuse it itself.
		if size := adoptRequestFor(path); size.Width != nil {
			base = referenceSize(modelFamily(cfg.Model), int(*size.Width), int(*size.Height), len(refs), typed)
		}
		plan.Sent = typed && base != [2]int{}
	default:
		plan.Source, base = "default 1:1", aspectRatios["1:1"]
	}
	if w <= 0 {
		plan.Width = base[0]
	}
	if h <= 0 {
		plan.Height = base[1]
	}
	return plan
}

// referenceSize is the size iris gives a render from its first reference of
// count, all of it from iris.c. Z-Image takes no references, so it has none.
func referenceSize(family string, w, h, count int, typed bool) [2]int {
	if family == "zimage" {
		return [2]int{}
	}
	if family != "flux3" {
		// iris_img2img and iris_multiref. In float32, as they are: it decides
		// the last pixel.
		if w > maxDimension || h > maxDimension {
			scale := float32(maxDimension) / float32(max(w, h))
			w, h = int(float32(w)*scale), int(float32(h)*scale)
		}
		w, h = w/dimStep*dimStep, h/dimStep*dimStep
		if typed {
			// Sent beside a typed side, so it has to be one iris takes.
			w, h = max(w, minDimension), max(h, minDimension)
		}
		return [2]int{w, h}
	}
	area := float64(w) * float64(h)
	limit := float64(maxDimension) / float64(max(w, h))
	if count > 1 {
		// flux3_prepare_reference: each is cropped to the grid, and the first
		// sets the size.
		scale := 1.0
		if w*h > flux3RefAreaMulti {
			scale = math.Sqrt(flux3RefAreaMulti / area)
		}
		if scale = min(scale, limit); scale < 1 {
			w, h = int(float64(w)*scale), int(float64(h)*scale)
		}
		return [2]int{max(w/dimStep*dimStep, minDimension), max(h/dimStep*dimStep, minDimension)}
	}
	// flux3_prepare_reference: one is padded to a canvas on the grid, which is
	// what has to fit, and the take is cropped back to the image. A size given
	// to iris turns the crop off, so the side not typed is the canvas's. The
	// back-off is iris's and cannot fire while sides stop at maxDimension.
	canvas := func(side int) int {
		return min(max((side+dimStep-1)/dimStep*dimStep, minDimension), maxDimension)
	}
	scale := min(1, math.Sqrt(flux3RefAreaSingle/area), limit)
	for {
		fw := max(int(math.Round(float64(w)*scale)), 1)
		fh := max(int(math.Round(float64(h)*scale)), 1)
		if canvas(fw)*canvas(fh) <= flux3RefAreaSingle {
			if typed {
				return [2]int{canvas(fw), canvas(fh)}
			}
			return [2]int{fw, fh}
		}
		scale *= 0.995
	}
}
