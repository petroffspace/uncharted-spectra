package main

// frames.go — cross-domain views over the canonical PCM buffer.
//
// Uncharted Spectra treats the PCM byte buffer as the single source of truth.
// Two interpretive frames are provided:
//
//   SpatialFrame  — the buffer seen as a 2D image: 3 bytes per pixel (B,G,R),
//                   row-major, TOP-DOWN logical coordinates. Used by modules
//                   in category "spatial" (the former DSP family).
//
//   TemporalFrame — the buffer seen as a multichannel sample stream, one
//                   normalized float64 per channel per frame. Used by modules
//                   in category "temporal" (the former image-filter family).
//
// Both frames round-trip losslessly when unmodified: PCM → frame → PCM
// reproduces the original bytes exactly, including trailing bytes that do
// not fit the frame's natural grid. This invariant is what lets a "spatial"
// filter and a "temporal" filter be applied to the same file in any order
// without drift.
//
// The spatial grid is derived from PCMState.BMPDimensions(), i.e. the SAME
// grid the user sees in the image box — so what you filter is what you saw.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log"
	"math"
)

// ---------------------------------------------------------------------------
// SpatialFrame — the buffer as a 2D image
// ---------------------------------------------------------------------------

type SpatialFrame struct {
	W, H    int
	RowSize int    // W * 3
	Px      []byte // H*RowSize bytes, row-major, row 0 = TOP
	tail    []byte // PCM bytes past the grid, carried verbatim
	origLen int    // original PCM length
}

// NewSpatialFrame builds a top-down spatial view of the PCM buffer.
func NewSpatialFrame(s *PCMState) (*SpatialFrame, error) {
	if len(s.Data) == 0 {
		return nil, errors.New("empty PCM buffer")
	}
	w, h := s.BMPDimensions()
	rowSize := w * 3
	usable := rowSize * h

	pad := (3 - len(s.Data)%3) % 3
	padded := make([]byte, len(s.Data)+pad)
	copy(padded, s.Data)

	// BMPDimensions guarantees w*h <= ceil(len/3), so usable <= len(padded).
	f := &SpatialFrame{W: w, H: h, RowSize: rowSize, origLen: len(s.Data)}
	f.Px = make([]byte, usable)
	for r := 0; r < h; r++ {
		src := (h - 1 - r) * rowSize // storage is bottom-up; logical is top-down
		copy(f.Px[r*rowSize:(r+1)*rowSize], padded[src:src+rowSize])
	}
	f.tail = padded[usable:]
	return f, nil
}

// ToPCM reassembles the buffer in storage order (bottom-up) and trims to the
// original length. Exact inverse of NewSpatialFrame when Px is unmodified.
func (f *SpatialFrame) ToPCM() []byte {
	out := make([]byte, 0, f.origLen)
	for r := f.H - 1; r >= 0; r-- {
		out = append(out, f.Px[r*f.RowSize:(r+1)*f.RowSize]...)
	}
	out = append(out, f.tail...)
	if len(out) > f.origLen {
		out = out[:f.origLen]
	}
	return out
}

func (f *SpatialFrame) Offset(x, y int) int { return y*f.RowSize + x*3 }

func (f *SpatialFrame) ClampXY(x, y int) (int, int) {
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x >= f.W {
		x = f.W - 1
	}
	if y >= f.H {
		y = f.H - 1
	}
	return x, y
}

// Pixel returns the B, G, R bytes at (x, y), clamped to the frame.
func (f *SpatialFrame) Pixel(x, y int) (b, g, r byte) {
	x, y = f.ClampXY(x, y)
	o := f.Offset(x, y)
	return f.Px[o], f.Px[o+1], f.Px[o+2]
}

func (f *SpatialFrame) SetPixel(x, y int, b, g, r byte) {
	x, y = f.ClampXY(x, y)
	o := f.Offset(x, y)
	f.Px[o], f.Px[o+1], f.Px[o+2] = b, g, r
}

// Plane extracts channel c (0=B, 1=G, 2=R) as float32 in [0,1], row-major
// top-down. Spatial filters work on planes so they can be applied per channel.
func (f *SpatialFrame) Plane(c int) []float32 {
	p := make([]float32, f.W*f.H)
	for y := 0; y < f.H; y++ {
		row := y * f.RowSize
		for x := 0; x < f.W; x++ {
			p[y*f.W+x] = float32(f.Px[row+x*3+c]) / 255.0
		}
	}
	return p
}

// SetPlane writes a plane back, clamping to [0,1] and rounding to bytes.
func (f *SpatialFrame) SetPlane(c int, p []float32) {
	for y := 0; y < f.H; y++ {
		row := y * f.RowSize
		for x := 0; x < f.W; x++ {
			v := p[y*f.W+x]
			if v < 0 {
				v = 0
			}
			if v > 1 {
				v = 1
			}
			f.Px[row+x*3+c] = byte(v*255.0 + 0.5)
		}
	}
}

// Luma returns perceived brightness per pixel in [0,1] (BT.601 weights).
// Useful for filters that want a single scalar field rather than 3 planes.
func (f *SpatialFrame) Luma() []float32 {
	p := make([]float32, f.W*f.H)
	for y := 0; y < f.H; y++ {
		row := y * f.RowSize
		for x := 0; x < f.W; x++ {
			o := row + x*3
			b := float32(f.Px[o]) / 255.0
			g := float32(f.Px[o+1]) / 255.0
			r := float32(f.Px[o+2]) / 255.0
			p[y*f.W+x] = 0.114*b + 0.587*g + 0.299*r
		}
	}
	return p
}

// ---------------------------------------------------------------------------
// TemporalFrame — the buffer as a multichannel sample stream
// ---------------------------------------------------------------------------

type TemporalFrame struct {
	SampleRate uint32
	Bits       uint16
	Channels   int
	Frames     int       // number of sample frames
	Ch         [][]float64 // Ch[c][i], normalized to [-1, 1]
	tail       []byte    // leftover bytes not forming a whole frame
	origLen    int
}

// NewTemporalFrame decodes the PCM buffer into normalized per-channel floats.
// Supported depths: 8 (unsigned), 16, 24, 32 (signed little-endian).
func NewTemporalFrame(s *PCMState) (*TemporalFrame, error) {
	if len(s.Data) == 0 {
		return nil, errors.New("empty PCM buffer")
	}
	bits := s.BitsPerSample
	bytesPer := int(bits) / 8
	if bytesPer == 0 {
		return nil, errors.New("unsupported bit depth")
	}
	ch := int(s.Channels)
	if ch == 0 {
		return nil, errors.New("zero channels")
	}
	frameBytes := ch * bytesPer
	n := len(s.Data) / frameBytes

	f := &TemporalFrame{
		SampleRate: s.SampleRate,
		Bits:       bits,
		Channels:   ch,
		Frames:     n,
		origLen:    len(s.Data),
		Ch:         make([][]float64, ch),
	}
	for c := range f.Ch {
		f.Ch[c] = make([]float64, n)
	}
	for i := 0; i < n; i++ {
		base := i * frameBytes
		for c := 0; c < ch; c++ {
			f.Ch[c][i] = decodeSample(s.Data[base+c*bytesPer:], bits)
		}
	}
	f.tail = s.Data[n*frameBytes:]
	return f, nil
}

// ToPCM re-encodes samples and appends the trailing bytes. Exact inverse of
// NewTemporalFrame when the sample values are unmodified.
func (f *TemporalFrame) ToPCM() []byte {
	bytesPer := int(f.Bits) / 8
	frameBytes := f.Channels * bytesPer
	out := make([]byte, f.Frames*frameBytes)
	for i := 0; i < f.Frames; i++ {
		base := i * frameBytes
		for c := 0; c < f.Channels; c++ {
			encodeSample(out[base+c*bytesPer:], f.Bits, f.Ch[c][i])
		}
	}
	out = append(out, f.tail...)
	if len(out) > f.origLen {
		out = out[:f.origLen]
	}
	return out
}

// Map applies fn to every sample of every channel, in place.
func (f *TemporalFrame) Map(fn func(v float64) float64) {
	for c := range f.Ch {
		for i := range f.Ch[c] {
			f.Ch[c][i] = fn(f.Ch[c][i])
		}
	}
}

// Peak returns the largest absolute sample value across all channels.
func (f *TemporalFrame) Peak() float64 {
	peak := 0.0
	for c := range f.Ch {
		for _, v := range f.Ch[c] {
			a := math.Abs(v)
			if a > peak {
				peak = a
			}
		}
	}
	return peak
}

// ---------------------------------------------------------------------------
// Sample codec — normalization is exact for all supported depths
// ---------------------------------------------------------------------------

func decodeSample(b []byte, bits uint16) float64 {
	switch bits {
	case 8:
		return (float64(b[0]) - 128.0) / 128.0
	case 16:
		return float64(int16(binary.LittleEndian.Uint16(b))) / 32768.0
	case 24:
		u := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
		if u&0x800000 != 0 {
			u |= 0xFF000000
		}
		return float64(int32(u)) / 8388608.0
	case 32:
		return float64(int32(binary.LittleEndian.Uint32(b))) / 2147483648.0
	}
	return 0
}

func encodeSample(b []byte, bits uint16, v float64) {
	switch bits {
	case 8:
		i := int(math.Round(v*128.0)) + 128
		if i < 0 {
			i = 0
		}
		if i > 255 {
			i = 255
		}
		b[0] = byte(i)
	case 16:
		i := int64(math.Round(v * 32768.0))
		if i > 32767 {
			i = 32767
		}
		if i < -32768 {
			i = -32768
		}
		binary.LittleEndian.PutUint16(b, uint16(int16(i)))
	case 24:
		i := int64(math.Round(v * 8388608.0))
		if i > 8388607 {
			i = 8388607
		}
		if i < -8388608 {
			i = -8388608
		}
		u := uint32(int32(i))
		b[0] = byte(u)
		b[1] = byte(u >> 8)
		b[2] = byte(u >> 16)
	case 32:
		i := int64(math.Round(v * 2147483648.0))
		if i > 2147483647 {
			i = 2147483647
		}
		if i < -2147483648 {
			i = -2147483648
		}
		binary.LittleEndian.PutUint32(b, uint32(int32(i)))
	}
}

// ---------------------------------------------------------------------------
// Module-facing helpers — the standard entry points for filter modules
// ---------------------------------------------------------------------------

// applySpatial gives fn a 2D view and writes the result back into s.
func applySpatial(s *PCMState, fn func(*SpatialFrame) error) error {
	f, err := NewSpatialFrame(s)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	s.Data = f.ToPCM()
	return nil
}

// applyTemporal gives fn a sample-stream view and writes the result back.
func applyTemporal(s *PCMState, fn func(*TemporalFrame) error) error {
	f, err := NewTemporalFrame(s)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	s.Data = f.ToPCM()
	return nil
}

// ---------------------------------------------------------------------------
// Diagnostic module — proves the lossless round-trip invariant at runtime
// ---------------------------------------------------------------------------

func probeFrames(s *PCMState) error {
	sf, err := NewSpatialFrame(s)
	if err != nil {
		return err
	}
	if !bytes.Equal(sf.ToPCM(), s.Data) {
		return errors.New("spatial frame round-trip MISMATCH")
	}
	tf, err := NewTemporalFrame(s)
	if err != nil {
		return err
	}
	if !bytes.Equal(tf.ToPCM(), s.Data) {
		return errors.New("temporal frame round-trip MISMATCH")
	}
	log.Printf("frame probe OK: spatial %dx%d (%d px) · temporal %d frames × %d ch @ %d-bit · both exact",
		sf.W, sf.H, sf.W*sf.H, tf.Frames, tf.Channels, tf.Bits)
	return nil
}

func init() {
	RegisterModule(&Module{
		Name:     "frame_probe",
		Category: "tool",
		Label:    "Frame Probe (diagnostic)",
		Params:   nil,
		Apply: func(s *PCMState, _ map[string]float64) error {
			return probeFrames(s)
		},
	})
}
