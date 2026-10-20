package main

// dsp_distortion.go — spatial waveshaper (clip / saturate / fold / posterize).
//
// A waveshaper maps input intensity x → output intensity f(x). Applied
// to the spatial plane with a drive stage (input scaling) and an optional
// makeup gain. Curves:
//
//   hard clip   y = clamp(drive·x, -1, 1)                    — flat tops
//   tanh sat    y = tanh(drive·x)                            — smooth crunch
//   foldback    y = drive·x folded into [-1,1] by reflection — metallic chaos
//   posterize   y = quantize(drive·x) to `levels` steps        — bitcrush /
//                                                             stark banding
//
// The plane is mapped from [0,1] to signed [-1,1] (pivot 0.5) around the
// midpoint before shaping and back after, so all four curves behave
// symmetrically as they do in audio-land. Repacking the shaped bytes
// means the audio inherits the equivalent nonlinear waveshape —
// posterize in particular produces classic sample-rate-y bitcrush tones.
//
// Params:
//   type    0=hard clip 1=tanh 2=foldback 3=posterize
//   drive   0..8 — pre-shape input gain
//   levels  2..64 — posterize step count (only used by type 3)
//   makeup  0..2 — post-shape output gain (linear)
//   mix     0..100 %

import (
	"math"
)

// foldIntoUnit reflects v into the [-1, 1] range like a bounced ray.
// Strictly odd-symmetric: f(-v) == -f(v).
func foldIntoUnit(v float64) float64 {
	if v < 0 {
		return -foldIntoUnit(-v)
	}
	// period 4: v -> triangular wave in [-1,1]
	m := math.Mod(v, 4.0)
	if m < 1 {
		return m
	}
	if m < 3 {
		return 2.0 - m
	}
	return m - 4.0
}

func shapeCurve(shapeType int, x, drive float64, levels int) float64 {
	v := x * drive
	switch shapeType {
	case 0: // hard clip
		return math.Max(-1, math.Min(1, v))
	case 1: // tanh saturation
		return math.Tanh(v)
	case 2: // foldback
		return foldIntoUnit(v)
	case 3: // posterize
		if levels < 2 {
			levels = 2
		}
		// quantize signed value to levels steps in [-1,1]
		q := float64(levels)
		return math.Round(v*q) / q
	default:
		return v
	}
}

func applySpatialDistortion(s *PCMState, params map[string]float64) error {
	shapeType := int(params["type"])
	drive := params["drive"]
	levels := int(math.Round(params["levels"]))
	makeup := params["makeup"]
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

	mix32 := float32(mix)
	dry32 := float32(1 - mix)

	return applySpatial(s, func(f *SpatialFrame) error {
		for c := 0; c < 3; c++ {
			p := f.Plane(c)
			for i := range p {
				// [0,1] → signed [-1,1]
				signed := float64(p[i])*2.0 - 1.0
				wet := shapeCurve(shapeType, signed, drive, levels) * makeup
				// back to [0,1]
				wet = wet*0.5 + 0.5
				p[i] = p[i]*dry32 + float32(wet)*mix32
			}
			f.SetPlane(c, p) // clamps to [0,1]
		}
		return nil
	})
}

func init() {
	RegisterModule(&Module{
		Name:     "spatial_distortion",
		Category: "sound", // renders under the "spatial" tab
		Label:    "Distortion (clip/sat/fold/posterize)",
		Params: []ParamSpec{
			{Name: "type", Label: "Curve (0=clip 1=sat 2=fold 3=post)", Min: 0, Max: 3, Step: 1, Def: 1, Kind: "select"},
			{Name: "drive", Label: "Drive", Min: 0, Max: 8, Step: 0.1, Def: 3, Kind: "number"},
			{Name: "levels", Label: "Posterize levels", Min: 2, Max: 64, Step: 1, Def: 4, Kind: "number"},
			{Name: "makeup", Label: "Makeup gain", Min: 0, Max: 2, Step: 0.05, Def: 1, Kind: "number"},
			{Name: "mix", Label: "Mix (%)", Min: 0, Max: 100, Step: 1, Def: 100, Kind: "number"},
		},
		Apply: applySpatialDistortion,
	})
}
