package main

// dsp_filter.go — spatial frequency filters (lowpass / highpass / bandpass).
//
// Implementation: separable 1D convolution, horizontal pass then vertical
// pass, per channel plane. Kernels are normalized gaussians; the highpass
// kernel is identity minus gaussian (sum 0 → DC removed); the bandpass is
// a difference of two gaussians one octave apart (sigma and 2·sigma).
//
// Edges are handled by clamping (replicate) — no border artifacts beyond
// a mild edge gathering.
//
// Params:
//   type   0 = lowpass (blur), 1 = highpass (sharpen/edges), 2 = bandpass
//   radius 1..25 — gaussian half-width in pixels; larger = stronger effect
//   mix    0..100 % dry/wet blend

import (
	"math"
)

// gaussianKernel returns a normalized discrete gaussian with half-width r
// (kernel length 2r+1) and standard deviation sigma.
func gaussianKernel(r int, sigma float64) []float64 {
	n := 2*r + 1
	k := make([]float64, n)
	sum := 0.0
	for i := 0; i < n; i++ {
		x := float64(i - r)
		k[i] = math.Exp(-(x * x) / (2 * sigma * sigma))
		sum += k[i]
	}
	for i := range k {
		k[i] /= sum
	}
	return k
}

// derivedKernel returns the kernel for the requested filter type and radius.
func derivedKernel(filterType int, radius int) []float64 {
	r := radius
	if r < 1 {
		r = 1
	}
	sigma := float64(r) / 2.0
	if sigma < 0.5 {
		sigma = 0.5
	}
	switch filterType {
	case 1: // highpass: delta − gaussian (zero-sum)
		g := gaussianKernel(r, sigma)
		k := make([]float64, len(g))
		for i := range g {
			k[i] = -g[i]
		}
		k[r] += 1.0
		return k
	case 2: // bandpass: gaussian(2σ) − gaussian(σ), centered, normalized to peak 1
		wide := gaussianKernel(2*r, 2*sigma)
		narrow := gaussianKernel(r, sigma)
		k := make([]float64, len(wide))
		for i := range k {
			wn, w := 0.0, wide[i]
			if i >= r && i < r+len(narrow) {
				wn = narrow[i-r]
			}
			k[i] = wn - w
		}
		// normalize by peak magnitude so wet signal stays in a sensible range
		peak := 0.0
		for _, v := range k {
			a := math.Abs(v)
			if a > peak {
				peak = a
			}
		}
		if peak > 0 {
			for i := range k {
				k[i] /= peak
			}
		}
		return k
	default: // lowpass: plain gaussian
		return gaussianKernel(r, sigma)
	}
}

// convolveSeparable runs k over src horizontally into tmp, then vertically
// into dst (dst may alias src). Edge strategy: replicate (clamp).
func convolveSeparable(src, dst, tmp []float32, w, h int, k []float64) {
	taps := len(k)
	r := taps / 2
	k32 := make([]float32, taps)
	for i := range k {
		k32[i] = float32(k[i])
	}

	// horizontal pass: src -> tmp
	for y := 0; y < h; y++ {
		row := y * w
		for x := 0; x < w; x++ {
			var acc float32
			for t := 0; t < taps; t++ {
				sx := x + t - r
				if sx < 0 {
					sx = 0
				}
				if sx >= w {
					sx = w - 1
				}
				acc += src[row+sx] * k32[t]
			}
			tmp[row+x] = acc
		}
	}

	// vertical pass: tmp -> dst
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var acc float32
			for t := 0; t < taps; t++ {
				sy := y + t - r
				if sy < 0 {
					sy = 0
				}
				if sy >= h {
					sy = h - 1
				}
				acc += tmp[sy*w+x] * k32[t]
			}
			dst[y*w+x] = acc
		}
	}
}

func applySpatialFilter(s *PCMState, params map[string]float64) error {
	filterType := int(params["type"])
	radius := int(math.Round(params["radius"]))
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

	k := derivedKernel(filterType, radius)
	mix32 := float32(mix)
	dry32 := float32(1 - mix)

	return applySpatial(s, func(f *SpatialFrame) error {
		n := f.W * f.H
		tmp := make([]float32, n)
		dst := make([]float32, n)
		for c := 0; c < 3; c++ {
			src := f.Plane(c)
			convolveSeparable(src, dst, tmp, f.W, f.H, k)
			for i := range src {
				src[i] = src[i]*dry32 + dst[i]*mix32
			}
			// SetPlane clamps out-of-range values (bandpass goes negative,
			// highpass overshoots) — negative spatial frequencies land at 0.
			f.SetPlane(c, src)
		}
		return nil
	})
}

func init() {
	RegisterModule(&Module{
		Name:     "spatial_filter",
		Category: "sound", // renders under the "spatial" tab
		Label:    "Frequency Filter (blur/sharpen/ring)",
		Params: []ParamSpec{
			{Name: "type", Label: "Filter type (0=low 1=high 2=band)", Min: 0, Max: 2, Step: 1, Def: 0, Kind: "select"},
			{Name: "radius", Label: "Radius (px)", Min: 1, Max: 25, Step: 1, Def: 4, Kind: "number"},
			{Name: "mix", Label: "Mix (%)", Min: 0, Max: 100, Step: 1, Def: 100, Kind: "number"},
		},
		Apply: applySpatialFilter,
	})
}
