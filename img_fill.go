package main

// img_fill.go — temporal entropy injection (region fill / salt dust).
//
// Injects fresh /dev/urandom (via the shared entropy source from
// rotary.go) into the sample stream.
//
// Modes:
//   region — floods the time-span [start, end] (fractions of total
//            duration, 0..1) with pure entropy. Audio: a burst of
//            full-bandwidth white noise in the chosen window. Image:
//            a band of noise across the rows covered by that span.
//   salt   — replaces each sample independently with probability p
//            (0..1). Low p = faint static seasoning; p → 1 = total
//            dissolution into noise. This is the temporal twin of the
//            randomize rotary knob, but per-sample rather than per-pixel,
//            so the spatial granularity differs.
//
// A "keep amplitude" option (normalize 0..1) crossfades between raw
// entropy and level-matched entropy: the injected noise is scaled to
// the RMS of the region it replaces (or the whole signal, for salt),
// so fills can sit IN the mix instead of on top of it.
//
// Params:
//   mode     0 = region, 1 = salt
//   start    0..1 — region start (fraction of total length)
//   end      0..1 — region end (fraction of total length)
//   density  0..1 — salt replacement probability (salt mode)
//   match    0..1 — 0 = raw noise, 1 = amplitude-matched noise
//   mix      0..100 % dry/wet
//
// Null contract: mix = 0 → byte-identical. Region mode with start == end
// → byte-identical at any mix. Salt mode with density 0 → byte-identical.

import (
	"math"
)

func fillClamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// rms computes the root-mean-square of a sample slice.
func rms(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, v := range xs {
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(xs)))
}

// uniformNoise fills p with uniform random bytes mapped to [-1,1] signed.
func uniformNoise(p []float64) {
	if len(p) == 0 {
		return
	}
	raw := make([]byte, len(p))
	randomBytes(raw) // shared /dev/urandom source (rotary.go)
	const inv = 2.0 / 255.0
	for i, b := range raw {
		p[i] = float64(b)*inv - 1.0
	}
}

func applyTemporalFill(s *PCMState, params map[string]float64) error {
	mode := int(math.Round(params["mode"]))
	start := fillClamp01(params["start"])
	end := fillClamp01(params["end"])
	density := fillClamp01(params["density"])
	match := fillClamp01(params["match"])
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
	if start > end {
		start, end = end, start
	}
	if mode == 0 && start == end {
		return nil
	}
	if mode == 1 && density == 0 {
		return nil
	}

	return applyTemporal(s, func(f *TemporalFrame) error {
		n := f.Frames
		if n < 2 {
			return nil
		}

		switch mode {
		case 0: // region fill
			i0 := int(start * float64(n))
			i1 := int(end * float64(n))
			if i1 > n {
				i1 = n
			}
			span := i1 - i0
			if span <= 0 {
				return nil // zero-width region: null by contract
			}

			noise := make([]float64, span)
			uniformNoise(noise)

			if match > 0 {
				// Level-match: scale noise to RMS of the region being
				// replaced, computed across all channels.
				var region []float64
				for c := 0; c < f.Channels; c++ {
					region = append(region, f.Ch[c][i0:i1]...)
				}
				target := rms(region)
				actual := rms(noise)
				if actual > 1e-9 {
					scale := target / actual
					for i := range noise {
						noise[i] *= scale
					}
				}
			}

			amount := mix // amount of noise blended in per sample
			for c := 0; c < f.Channels; c++ {
				ch := f.Ch[c]
				for i := 0; i < span; i++ {
					x := float64(i0+i)
					if x >= float64(n) {
						break
					}
					ch[i0+i] = ch[i0+i]*(1-amount) + noise[i]*amount
				}
			}
			return nil

		default: // salt dust
			if density <= 0 {
				return nil // zero density: null by contract
			}

			// Per-sample Bernoulli trial using entropy bytes directly:
			// byte < density*256 ⇒ replace this sample.
			decide := make([]byte, n)
			randomBytes(decide)
			thresh := byte(density * 255.0)

			// Amplitude matching for salt: match to global RMS so the
			// seasoning sits at the signal's own level.
			var scale = 1.0
			if match > 0 {
				var all []float64
				for c := 0; c < f.Channels; c++ {
					all = append(all, f.Ch[c]...)
				}
				target := rms(all)
				noise := make([]float64, 64)
				uniformNoise(noise)
				actual := rms(noise)
				if actual > 1e-9 {
					scale = target / actual
				}
			}

			amount := mix
			noiseBuf := make([]float64, 256)
			nb := 0
			for c := 0; c < f.Channels; c++ {
				ch := f.Ch[c]
				for i := 0; i < n; i++ {
					if decide[i] < thresh {
						if nb == 0 {
							uniformNoise(noiseBuf)
							nb = len(noiseBuf)
						}
						v := noiseBuf[len(noiseBuf)-nb] * scale
						nb--
						ch[i] = ch[i]*(1-amount) + v*amount
					}
				}
			}
			return nil
		}
	})
}

func init() {
	RegisterModule(&Module{
		Name:     "temporal_fill",
		Category: "image", // renders under the "temporal" tab
		Label:    "Fill Random (region/salt entropy)",
		Params: []ParamSpec{
			{Name: "mode", Label: "Mode (0=region 1=salt)", Min: 0, Max: 1, Step: 1, Def: 0, Kind: "select"},
			{Name: "start", Label: "Region start (0..1)", Min: 0, Max: 1, Step: 0.01, Def: 0.25, Kind: "number"},
			{Name: "end", Label: "Region end (0..1)", Min: 0, Max: 1, Step: 0.01, Def: 0.35, Kind: "number"},
			{Name: "density", Label: "Salt density (0..1)", Min: 0, Max: 1, Step: 0.01, Def: 0.1, Kind: "number"},
			{Name: "match", Label: "Amplitude match (0..1)", Min: 0, Max: 1, Step: 0.01, Def: 0, Kind: "number"},
			{Name: "mix", Label: "Mix (%)", Min: 0, Max: 100, Step: 1, Def: 100, Kind: "number"},
		},
		Apply: applyTemporalFill,
	})
}
