package main

// dsp_delay.go — spatial multi-tap delay (ghosting / echo / comb).
//
// Each output pixel is the input pixel plus delayed copies of itself at
// offsets t·d (t = 1..taps), each weighted fb^t. Offsets can be laid out
// along the raster order (diagonal trails), horizontally, or vertically.
//
// In raster order an offset of d pixels IS an offset of 3·d bytes in the
// PCM stream — so the painted ghost trail corresponds sample-exactly to
// an audio echo. The frequency-domain consequence is comb filtering:
// notches at every 1/(3·d) cycles-per-byte.
//
// Params:
//   type      0 = raster (diagonal trails), 1 = horizontal, 2 = vertical
//   distance  1..256 px — tap spacing d
//   feedback  0..0.95 — tap weight ratio; geometric decay of echo tails
//   taps      1..32 — number of echo copies (compute cost scales with this)
//   mix       0..100 %
//
// Edge strategy: replicate (clamped index) — echoes gather at borders.

import (
	"math"
)

// delayIndex returns the clamped source index for a delay offset applied
// to linear index i in a w×h plane, depending on direction mode.
func delayIndex(i, w, h, d, mode int) int {
	switch mode {
	case 1: // horizontal: same row, shifted columns
		x := i % w
		y := i / w
		x -= d
		if x < 0 {
			x = 0
		}
		return y*w + x
	case 2: // vertical: same column, shifted rows
		x := i % w
		y := i/w - d
		if y < 0 {
			y = 0
		}
		return y*w + x
	default: // raster: straight linear offset (diagonal trail)
		j := i - d
		if j < 0 {
			j = 0
		}
		return j
	}
}

func applySpatialDelay(s *PCMState, params map[string]float64) error {
	mode := int(params["type"])
	dist := int(math.Round(params["distance"]))
	fb := params["feedback"]
	taps := int(math.Round(params["taps"]))
	mix := params["mix"] / 100.0
	if mix < 0 {
		mix = 0
	}
	if mix > 1 {
		mix = 1
	}
	if mix == 0 {
		return nil
	}
	if dist < 1 {
		dist = 1
	}
	if taps < 1 {
		taps = 1
	}
	if fb < 0 {
		fb = 0
	}
	if fb > 0.95 {
		fb = 0.95
	}

	mix32 := float32(mix)
	dry32 := float32(1 - mix)

	return applySpatial(s, func(f *SpatialFrame) error {
		n := f.W * f.H
		for c := 0; c < 3; c++ {
			src := f.Plane(c)
			out := make([]float32, n)
			copy(out, src)

			for t := 1; t <= taps; t++ {
				w := math.Pow(fb, float64(t))
				if w < 1e-4 {
					break // tail decayed to inaudibility
				}
				w32 := float32(w)
				off := t * dist
				for i := 0; i < n; i++ {
					out[i] += w32 * src[delayIndex(i, f.W, f.H, off, mode)]
				}
			}

			for i := range src {
				src[i] = src[i]*dry32 + out[i]*mix32
			}
			f.SetPlane(c, src) // clamps; summed echoes can exceed 1
		}
		return nil
	})
}

func init() {
	RegisterModule(&Module{
		Name:     "spatial_delay",
		Category: "sound", // renders under the "spatial" tab
		Label:    "Multi-Tap Delay (echo/ghosting)",
		Params: []ParamSpec{
			{Name: "type", Label: "Direction (0=raster 1=horiz 2=vert)", Min: 0, Max: 2, Step: 1, Def: 0, Kind: "select"},
			{Name: "distance", Label: "Tap distance (px)", Min: 1, Max: 256, Step: 1, Def: 16, Kind: "number"},
			{Name: "feedback", Label: "Feedback (tap decay)", Min: 0, Max: 0.95, Step: 0.01, Def: 0.5, Kind: "number"},
			{Name: "taps", Label: "Taps", Min: 1, Max: 32, Step: 1, Def: 8, Kind: "number"},
			{Name: "mix", Label: "Mix (%)", Min: 0, Max: 100, Step: 1, Def: 100, Kind: "number"},
		},
		Apply: applySpatialDelay,
	})
}
