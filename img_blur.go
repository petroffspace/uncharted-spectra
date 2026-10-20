package main

// img_blur.go — temporal gaussian blur (audio-stream smoothing).
//
// A gaussian blur along the time axis IS a lowpass filter: each sample
// becomes the weighted average of its temporal neighborhood. The gaussian
// kernel is approximated by three consecutive box blurs (sliding-window,
// O(n) per pass regardless of radius) — the standard fast approximation.
//
// Spatial/audio cross-view:
//   small radius  → tape hiss softened, cymbal fizz rounded (gentle LP)
//   medium radius → smeared, underwater, ambient wash
//   large radius  → signal dissolves toward its mean — a slow drone
//
// Per-channel operation; edges use replicate (clamped) windows.
//
// Params:
//   radius  1..16384 — blur half-width in SAMPLES (at 44.1 kHz, 64 ≈ 1.5 ms,
//                       4096 ≈ 93 ms, 16384 ≈ 372 ms)
//   mix     0..100 % dry/wet
//
// Null contract: mix = 0 → byte-identical output.
//                 radius = 1, mix = 100 → box of 3 taps ×3 passes ≈ mild
//                 smoothing only (never an exact identity; use mix 0 for
//                 the strict null test).

import (
	"math"
)

// boxBlur1D averages a sliding window of half-width r over src into dst.
// Replicate edges. O(n): a running accumulator, adding the entering
// sample and subtracting the leaving one.
func boxBlur1D(src, dst []float64, r int) {
	n := len(src)
	if r < 1 || n == 0 {
		copy(dst, src)
		return
	}
	if r >= n {
		r = n - 1
	}

	var acc float64
	// Seed accumulator with the first window [0, r] (clamped).
	for i := 0; i <= r && i < n; i++ {
		acc += src[i]
	}
	win := r + 1 // number of samples currently accumulated

	for i := 0; i < n; i++ {
		dst[i] = acc / float64(win)
		// Advance window: add src[i+r+1] if it exists, drop src[i-r] if
		// it is still inside the array (replicate strategy keeps win
		// growing until both edges are real samples).
		add := i + r + 1
		if add < n {
			acc += src[add]
			win++
		}
		drop := i - r
		if drop >= 0 {
			acc -= src[drop]
			win--
		}
	}
}

// gaussianApprox runs three box blurs (src → a → b → dst) to approximate
// a gaussian with effective sigma ≈ r·sqrt(3/4·(3-2/r))… (standard result:
// three boxes match the second moment of a gaussian of similar extent).
func gaussianApprox(src, tmp, tmp2, dst []float64, r int) {
	boxBlur1D(src, tmp, r)
	boxBlur1D(tmp, tmp2, r)
	boxBlur1D(tmp2, dst, r)
}

func applyTemporalBlur(s *PCMState, params map[string]float64) error {
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
	if radius < 1 {
		radius = 1
	}
	if radius > 16384 {
		radius = 16384
	}

	return applyTemporal(s, func(f *TemporalFrame) error {
		n := f.Frames
		if n < 4 {
			return nil
		}

		for c := 0; c < f.Channels; c++ {
			ch := f.Ch[c]

			// Signed samples are already centered on 0, so blurring
			// directly preserves DC correctly — no mean handling needed
			// (unlike the spatial planes).

			tmp := make([]float64, n)
			tmp2 := make([]float64, n)
			blurred := make([]float64, n)

			gaussianApprox(ch, tmp, tmp2, blurred, radius)

			for i := range ch {
				ch[i] = ch[i]*(1-mix) + blurred[i]*mix
			}
		}
		return nil
	})
}

func init() {
	RegisterModule(&Module{
		Name:     "temporal_blur",
		Category: "image", // renders under the "temporal" tab
		Label:    "Gaussian Blur (temporal smoothing)",
		Params: []ParamSpec{
			{Name: "radius", Label: "Radius (samples)", Min: 1, Max: 16384, Step: 1, Def: 64, Kind: "number"},
			{Name: "mix", Label: "Mix (%)", Min: 0, Max: 100, Step: 1, Def: 100, Kind: "number"},
		},
		Apply: applyTemporalBlur,
	})
}
