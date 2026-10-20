package main

// img_adjust.go — temporal adjustments (brightness/contrast/levels on audio).
//
// Applies classic photo-adjustment mathematics to the sample stream.
// Since TemporalFrame holds samples in [-1,1] (signed), we map them to
// unsigned [0,1] for the photo math, then back to signed.
//
// Effects:
//   brightness  — DC offset shift (lifts/drops the entire waveform vertically)
//                 → "gray level" adjustment in the image interpretation
//   contrast    — expansion/compression around a pivot point
//                 → dynamics widening or squashing
//   levels      — hard threshold gate: clips samples outside [in_lo, in_hi]
//                 → brutally effective noise gate on audio
//                 (also maps output to [out_lo, out_hi] for stretch)
//
// Null contract: mix = 0 → byte-identical output.

import (
)

func taclamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// signedToUnsigned maps [-1,1] → [0,1]
func s2u(v float64) float64 {
	return (v + 1.0) * 0.5
}

// unsignedToSigned maps [0,1] → [-1,1]
func u2s(v float64) float64 {
	return v*2.0 - 1.0
}

func applyTemporalAdjust(s *PCMState, params map[string]float64) error {
	// Brightness/offset control (mapped to unsigned space: 0.5 = zero)
	brightness := params["brightness"] // -1..1 mapped as offset from 0.5

	// Contrast/expansion around pivot
	contrast := params["contrast"] // 0..2, where 1.0 = unity
	pivot := params["pivot"]       // 0..1 in unsigned (0.5 = center)

	// Levels/gate thresholds (unsigned space)
	inLo := params["in_lo"]
	inHi := params["in_hi"]
	outLo := params["out_lo"]
	outHi := params["out_hi"]

	// Dry/wet mix
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

	// Clamp parameters to valid ranges
	brightness = taclamp(brightness, -1, 1)
	contrast = taclamp(contrast, 0, 2)
	pivot = taclamp(pivot, 0, 1)
	inLo = taclamp(inLo, 0, 1)
	inHi = taclamp(inHi, 0, 1)
	outLo = taclamp(outLo, 0, 1)
	outHi = taclamp(outHi, 0, 1)

	// Ensure in_lo <= in_hi (user error tolerance)
	if inLo > inHi {
		inLo, inHi = inHi, inLo
	}
	if outLo > outHi {
		outLo, outHi = outHi, outLo
	}

	return applyTemporal(s, func(f *TemporalFrame) error {
		// Apply adjustment to each channel independently
		for c := 0; c < f.Channels; c++ {
			ch := f.Ch[c]
			for i := range ch {
				// Convert to unsigned [0,1] for photo math
				u := s2u(ch[i])

				// Save original for mixing
				orig := u

				// Stage 1: Brightness (additive offset)
				u = u + brightness
				if u < 0 {
					u = 0
				}
				if u > 1 {
					u = 1
				}

				// Stage 2: Contrast (multiplicative expansion around pivot)
				d := u - pivot
				u = pivot + d*contrast
				if u < 0 {
					u = 0
				}
				if u > 1 {
					u = 1
				}

				// Stage 3: Levels (hard threshold gate)
				if u < inLo {
					// Map below-threshold to outLo
					u = outLo
				} else if u > inHi {
					// Map above-threshold to outHi
					u = outHi
				} else {
					// Within range: linear interpolation from [inLo,inHi] to [outLo,outHi]
					rangeIn := inHi - inLo
					rangeOut := outHi - outLo
					if rangeIn > 0 {
						ratio := (u - inLo) / rangeIn
						u = outLo + ratio*rangeOut
					}
				}

				// Mix wet/dry
				final := orig*(1-mix) + u*mix

				// Clamp final output
				final = taclamp(final, 0, 1)

				// Convert back to signed [-1,1]
				ch[i] = u2s(final)
			}
		}
		return nil
	})
}

func init() {
	RegisterModule(&Module{
		Name:     "temporal_adjust",
		Category: "image", // renders under the "temporal" tab
		Label:    "Photo Adjust (brightness/contrast/levels)",
		Params: []ParamSpec{
			{Name: "brightness", Label: "Brightness (-1..1)", Min: -1, Max: 1, Step: 0.01, Def: 0, Kind: "number"},
			{Name: "contrast", Label: "Contrast (0..2)", Min: 0, Max: 2, Step: 0.01, Def: 1, Kind: "number"},
			{Name: "pivot", Label: "Contrast pivot", Min: 0, Max: 1, Step: 0.01, Def: 0.5, Kind: "number"},
			{Name: "in_lo", Label: "Input low threshold", Min: 0, Max: 1, Step: 0.01, Def: 0, Kind: "number"},
			{Name: "in_hi", Label: "Input high threshold", Min: 0, Max: 1, Step: 0.01, Def: 1, Kind: "number"},
			{Name: "out_lo", Label: "Output low", Min: 0, Max: 1, Step: 0.01, Def: 0, Kind: "number"},
			{Name: "out_hi", Label: "Output high", Min: 0, Max: 1, Step: 0.01, Def: 1, Kind: "number"},
			{Name: "mix", Label: "Mix (%)", Min: 0, Max: 100, Step: 1, Def: 100, Kind: "number"},
		},
		Apply: applyTemporalAdjust,
	})
}
