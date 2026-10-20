package main

// dsp_gain.go — spatial gain (exposure/brightness) module.
//
// Multiplies pixel intensity by 10^(dB/20) per selected channel(s),
// with a soft-clip ceiling and a dry/wet mix.
//
// Since the pixel bytes ARE the PCM stream, boosting the image also
// boosts waveform amplitude — the effect is audible and visible at once.
//
// Params:
//   gain_db     -40..+24 dB, linear multiplier on intensity
//   channel     0=all channels, 1=Blue only, 2=Green only, 3=Red only
//               (remember: BMP stores B,G,R order; label uses BMP order)
//   knee        soft-clip threshold 0.5..1.0 — above this, values ease
//               toward white instead of hitting a hard 255 wall
//   mix         0..100 % dry/wet blend (0 = no change)

import (
	"math"
)

func softClipKnee(v, knee float64) float64 {
	if v <= knee {
		return v
	}
	// Above the knee: tangent compression approaching 1.0 asymptotically.
	// At v == knee the slope is continuous; v >= large converges to 1.
	return knee + (1-knee)*math.Tanh((v-knee)/(1-knee))
}

func applySpatialGain(s *PCMState, params map[string]float64) error {
	gainDB := params["gain_db"]
	channel := int(params["channel"])
	knee := params["knee"]
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
	if knee < 0.5 {
		knee = 0.5
	}
	if knee > 1 {
		knee = 1
	}

	factor := math.Pow(10, gainDB/20.0)

	return applySpatial(s, func(f *SpatialFrame) error {
		// Which planes does this apply to?
		doPlane := [3]bool{false, false, false}
		switch channel {
		case 0:
			doPlane = [3]bool{true, true, true}
		case 1:
			doPlane = [3]bool{true, false, false} // B
		case 2:
			doPlane = [3]bool{false, true, false} // G
		case 3:
			doPlane = [3]bool{false, false, true} // R
		default:
			doPlane = [3]bool{true, true, true}
		}

		for c := 0; c < 3; c++ {
			if !doPlane[c] {
				continue
			}
			p := f.Plane(c)
			mix32 := float32(mix)
			dry32 := float32(1 - mix)
			for i := range p {
				wet := float32(softClipKnee(float64(p[i])*factor, knee))
				p[i] = p[i]*dry32 + wet*mix32
			}
			f.SetPlane(c, p)
		}
		return nil
	})
}

func init() {
	RegisterModule(&Module{
		Name:     "spatial_gain",
		Category: "sound", // renders under the "spatial" tab
		Label:    "Gain / Exposure",
		Params: []ParamSpec{
			{Name: "gain_db", Label: "Gain (dB)", Min: -40, Max: 24, Step: 0.5, Def: 6, Kind: "number"},
			{Name: "channel", Label: "Channel (0=all 1=B 2=G 3=R)", Min: 0, Max: 3, Step: 1, Def: 0, Kind: "select"},
			{Name: "knee", Label: "Soft-clip knee", Min: 0.5, Max: 1, Step: 0.01, Def: 0.9, Kind: "number"},
			{Name: "mix", Label: "Mix (%)", Min: 0, Max: 100, Step: 1, Def: 100, Kind: "number"},
		},
		Apply: applySpatialGain,
	})
}
