package main

// dsp_reverb.go — spatial reverb, v2 (instrumented).
//
// Freeverb-style: parallel damped-feedback comb bank (prime-spaced
// delays scaled by room size) → series unity-gain allpass diffusers.
// Plane mean is removed before the resonators and restored after.
//
// v2 additions:
//   - Label carries a version marker ("v2") so the running build is
//     visible in the UI.
//   - Logs min/max/mean of every output plane — diagnostics.
//   - Wet path hard-clamped to [0,1] before mixing, so out-of-range
//     wet values can never dominate the dry signal.
//
// Null test contract: wet = 0 must leave the state byte-identical
// (dry path only). Any wet/size/damp values must stay within [0,1].

import (
	"log"
	"math"
)

var reverbCombDelays = []int{29, 37, 41, 43, 47, 53, 59, 61}
var reverbAllpassDelays = []int{23, 31, 41, 53, 67}

// combProcess runs one Freeverb comb: circular delay buffer with
// one-pole damped feedback. Stable for |fb| < 1.
func combProcess(in []float32, delay int, dampCoef, fb float64) []float32 {
	out := make([]float32, len(in))
	if delay < 1 {
		copy(out, in)
		return out
	}
	buf := make([]float32, delay)
	var store float32
	damp32 := float32(dampCoef)
	invDamp32 := float32(1 - dampCoef)
	fb32 := float32(fb)
	pos := 0
	for i, x := range in {
		y := buf[pos]
		store = y*damp32 + store*invDamp32
		buf[pos] = x + store*fb32
		out[i] = y
		pos++
		if pos == delay {
			pos = 0
		}
	}
	return out
}

// allpassProcess runs one Freeverb allpass (0.5 feedback, unity gain).
func allpassProcess(in []float32, delay int) []float32 {
	out := make([]float32, len(in))
	if delay < 1 {
		copy(out, in)
		return out
	}
	const fb = float32(0.5)
	buf := make([]float32, delay)
	pos := 0
	for i, x := range in {
		y := buf[pos]
		out[i] = -x + y
		buf[pos] = x + y*fb
		pos++
		if pos == delay {
			pos = 0
		}
	}
	return out
}

func rvclamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func applySpatialReverb(s *PCMState, params map[string]float64) error {
	size := rvclamp(params["size"], 0, 1)
	dampParam := rvclamp(params["damp"], 0, 1)
	wet := rvclamp(params["wet"], 0, 1)
	gain := rvclamp(params["gain"], 0, 2)
	if wet == 0 && gain == 1.0 {
		return nil
	}

	scale := 2 + int(math.Round(size*8)) // 2..10
	fb := 0.84 + 0.14*size              // 0.84..0.98
	dampCoef := dampParam * 0.4

	const wetTrim = 0.6

	return applySpatial(s, func(f *SpatialFrame) error {
		n := f.W * f.H
		if n < 8 {
			return nil
		}

		maxDelay := n / 4
		if maxDelay < 1 {
			maxDelay = 1
		}

		for c := 0; c < 3; c++ {
			in := f.Plane(c)

			mean := 0.0
			for _, v := range in {
				mean += float64(v)
			}
			mean /= float64(len(in))
			mean32 := float32(mean)

			zero := make([]float32, n)
			for i, v := range in {
				zero[i] = v - mean32
			}

			// Parallel comb bank, averaged.
			wetSig := make([]float32, n)
			for _, d := range reverbCombDelays {
				dl := d * scale
				if dl > maxDelay {
					dl = maxDelay
				}
				o := combProcess(zero, dl, dampCoef, fb)
				for i := range wetSig {
					wetSig[i] += o[i]
				}
			}
			inv := float32(1.0 / float64(len(reverbCombDelays)))
			for i := range wetSig {
				wetSig[i] *= inv
			}

			// Series allpass diffusers.
			for _, d := range reverbAllpassDelays {
				dl := d * scale
				if dl > maxDelay {
					dl = maxDelay
				}
				wetSig = allpassProcess(wetSig, dl)
			}

			// Mix with hard-clamped wet path.
			dryScale := float32((1 - wet) * gain)
			wet32 := float32(wet)
			for i := range in {
				wetV := float32(rvclamp((mean+float64(wetSig[i]))*wetTrim, 0, 1))
				in[i] = in[i]*dryScale + wet32*wetV
			}

			// Diagnostics: prove what this build actually computed.
			mn, mx, sum := 1e9, -1e9, 0.0
			for _, v := range in {
				vf := float64(v)
				if vf < mn {
					mn = vf
				}
				if vf > mx {
					mx = vf
				}
				sum += vf
			}
			log.Printf("reverb v2: plane %d: n=%d mean=%.4f min=%.4f max=%.4f (size=%.2f damp=%.2f wet=%.2f fb=%.2f)",
				c, n, sum/float64(n), mn, mx, size, dampParam, wet, fb)

			f.SetPlane(c, in)
		}
		return nil
	})
}

func init() {
	RegisterModule(&Module{
		Name:     "spatial_reverb",
		Category: "sound", // renders under the "spatial" tab
		Label:    "Reverb (comb/allpass diffusion) v2",
		Params: []ParamSpec{
			{Name: "size", Label: "Room size", Min: 0, Max: 1, Step: 0.01, Def: 0.5, Kind: "number"},
			{Name: "damp", Label: "Damping", Min: 0, Max: 1, Step: 0.01, Def: 0.5, Kind: "number"},
			{Name: "wet", Label: "Wet (0..1)", Min: 0, Max: 1, Step: 0.01, Def: 0.4, Kind: "number"},
			{Name: "gain", Label: "Dry gain", Min: 0, Max: 2, Step: 0.05, Def: 1, Kind: "number"},
		},
		Apply: applySpatialReverb,
	})
}
