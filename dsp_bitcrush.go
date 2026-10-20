package main

// dsp_bitcrush.go — spatial decimation + quantization (mosaic / crush).
//
// Two stages over the raster order of each plane:
//
//   decimate: plane value is sampled on a lattice of every `rate`-th pixel
//             and held to the next lattice point (sample & hold). Spatially
//             this tiles the image into rate×rate blocks sharing one value
//             (mosaic); temporally it is literally sample-rate reduction —
//             the audio gets stepped staircases with aliasing fold-over.
//
//   quantize: held values snapped to 2^bits levels across [0,1].
//             Spatial: hard tonal banding. Temporal: amplitude bitcrush.
//
// Byproduct honesty: decimation runs in raster order, so a block column
// crosses byte positions ≡ 0,1,2 mod 3 in the PCM stream — channels skew
// subtly per block. That asymmetry is inherent to cross-domain crushing;
// embraced, not fought.
//
// Params:
//   rate  1..64 — decimation lattice spacing (px / samples)
//   bits  1..8  — quantization bit depth after decimation
//   mix   0..100 % dry/wet
//
// Null contract: mix = 0 → byte-identical output. rate = 1, bits = 8,
// mix = 100 → also (nearly) identity: 8-bit quantization of an 8-bit
// plane is exact.

import (
	"math"
)

func bcclamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func applySpatialBitcrush(s *PCMState, params map[string]float64) error {
	rate := int(math.Round(bcclamp(params["rate"], 1, 64)))
	bits := int(math.Round(bcclamp(params["bits"], 1, 8)))
	mix := bcclamp(params["mix"], 0, 100) / 100.0
	if rate < 1 {
		rate = 1
	}
	if bits < 1 {
		bits = 1
	}
	if bits > 8 {
		bits = 8
	}
	if mix == 0 {
		return nil
	}

	quantLevels := float64(int(1) << uint(bits) - 1) // 2^bits - 1 steps
	invLevels := 1.0 / quantLevels
	mix32 := float32(mix)
	dry32 := float32(1 - mix)

	return applySpatial(s, func(f *SpatialFrame) error {
		for c := 0; c < 3; c++ {
			in := f.Plane(c)

			// Sample & hold over the raster, in place is impossible while
			// reading ahead within the same buffer — use a held-value walk.
			var held float64
			for i := range in {
				if i%rate == 0 {
					held = float64(in[i])
				}
				// Quantize the held value.
				wet := float32(math.Floor(held*quantLevels+0.5) * invLevels)
				in[i] = in[i]*dry32 + wet*mix32
			}
			f.SetPlane(c, in) // clamps; quantized values are already in range
		}
		return nil
	})
}

func init() {
	RegisterModule(&Module{
		Name:     "spatial_bitcrush",
		Category: "sound", // renders under the "spatial" tab
		Label:    "Bitcrush (decimate/quantize)",
		Params: []ParamSpec{
			{Name: "rate", Label: "Decimation rate (px)", Min: 1, Max: 64, Step: 1, Def: 4, Kind: "number"},
			{Name: "bits", Label: "Bit depth", Min: 1, Max: 8, Step: 1, Def: 4, Kind: "number"},
			{Name: "mix", Label: "Mix (%)", Min: 0, Max: 100, Step: 1, Def: 100, Kind: "number"},
		},
		Apply: applySpatialBitcrush,
	})
}
