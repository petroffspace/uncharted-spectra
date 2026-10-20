package main

// Uncharted Spectra — core engine server.
//
// Single-user, single-session. Holds one canonical PCM state per session,
// renders it as WAV, BMP, and waveform peaks. All mutations (rotaries,
// filters, scans) go through the module registry and are undoable.
//
// Endpoints:
//   POST /api/upload        multipart file field "file" (.wav or .bmp)
//   GET  /api/state         metadata + waveform peaks + undo availability
//   GET  /audio/current.wav current state as WAV
//   GET  /image/current.bmp current state as BMP
//   POST /api/undo          pop history stack
//   POST /api/revert        back to upload state ("Revert to base")
//   GET  /api/modules       list registered modules
//   POST /api/module/apply  {module, params} — apply a registered module
//   POST /api/scan/start    begin/resume scanning (PHASE 9)
//   POST /api/scan/stop     halt scanning (finding retained)
//   GET  /api/scan/status   scanner state, structure index, segment count
//   POST /api/scan/keep     apply best finding as one undoable mutation
//   GET  /*                 static files from ./web/

import (
	"bytes"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed all:web
var embeddedWeb embed.FS

const (
	listenAddr         = "127.0.0.1:8484"
	maxUpload          = 256 << 20 // 256 MB (Phase 10.1)
	maxHistory         = 6         // reduced for 256 MB cap (Phase 10.1)
	peakBuckets        = 1600
	maxSamplesInBucket = 256 // sample instead of walking full bucket (Phase 10.1)
)

// ---------------------------------------------------------------------------
// PCM state — the single source of truth. BMP and WAV are just views of it.
// ---------------------------------------------------------------------------

type PCMState struct {
	Data          []byte
	SampleRate    uint32
	Channels      uint16
	BitsPerSample uint16
}

func (p *PCMState) Clone() PCMState {
	c := PCMState{
		SampleRate:    p.SampleRate,
		Channels:      p.Channels,
		BitsPerSample: p.BitsPerSample,
	}
	c.Data = make([]byte, len(p.Data))
	copy(c.Data, p.Data)
	return c
}

func (p *PCMState) DurationSec() float64 {
	bytesPerFrame := int(p.Channels) * int(p.BitsPerSample) / 8
	if bytesPerFrame == 0 || p.SampleRate == 0 {
		return 0
	}
	return float64(len(p.Data)) / float64(bytesPerFrame) / float64(p.SampleRate)
}

// BMP view dimensions, mirroring wav2bmp: pad to multiple of 3, near-square
// width with width*3 divisible by 4 (so no row padding ever exists).
func (p *PCMState) BMPDimensions() (w, h int) {
	l := len(p.Data)
	pad := (3 - l%3) % 3
	totalPixels := (l + pad) / 3
	w = int(math.Sqrt(float64(totalPixels)))
	for w > 0 && (w*3)%4 != 0 {
		w--
	}
	if w < 1 {
		w = 1
	}
	h = totalPixels / w
	if h < 1 {
		h = 1
	}
	return w, h
}

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

// PresetStep records one applied module configuration for preset export/import.
type PresetStep struct {
	Module string             `json:"module"`
	Params map[string]float64 `json:"params"`
}

func cloneChain(c []PresetStep) []PresetStep {
	if len(c) == 0 {
		return nil
	}
	out := make([]PresetStep, len(c))
	for i, step := range c {
		pm := make(map[string]float64, len(step.Params))
		for k, v := range step.Params {
			pm[k] = v
		}
		out[i] = PresetStep{Module: step.Module, Params: pm}
	}
	return out
}

type Session struct {
	mu           sync.Mutex
	base         PCMState       // upload state (Revert target)
	current      PCMState       // live state
	history      []PCMState     // undo stack, newest last
	chain        []PresetStep   // applied effect chain since upload
	chainHistory [][]PresetStep // undo stack for effect chain
	fileName     string
	kind         string // "wav" or "bmp"
	hasState     bool
}

func (s *Session) stemName() string {
	base := filepath.Base(s.fileName)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" {
		return "uncharted-spectra"
	}
	return stem
}

var sess = &Session{}

// ---------------------------------------------------------------------------
// Module registry. Future files (rotary.go, filters, dsp_*.go, scan.go)
// call RegisterModule from init(). Core knows nothing about them.
// ---------------------------------------------------------------------------

type ParamSpec struct {
	Name  string  `json:"name"`
	Label string  `json:"label"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Step  float64 `json:"step"`
	Def   float64 `json:"def"`
	Kind  string  `json:"kind"` // "number" | "toggle" | "select"
}

type Module struct {
	Name     string      `json:"name"`
	Category string      `json:"category"` // "sound" | "image" | "tool"
	Label    string      `json:"label"`
	Params   []ParamSpec `json:"params"`
	// Hidden modules are excluded from /api/modules (palette listing)
	// but remain dispatchable by name. (PHASE 9)
	Hidden bool `json:"hidden"`
	// Apply mutates s in place. Params keys match ParamSpec.Name.
	Apply func(s *PCMState, params map[string]float64) error `json:"-"`
}

var (
	modulesMu sync.RWMutex
	modules   = map[string]*Module{}
)

func RegisterModule(m *Module) {
	modulesMu.Lock()
	defer modulesMu.Unlock()
	modules[m.Name] = m
	log.Printf("module registered: %s (%s)", m.Name, m.Category)
}

func getModule(name string) (*Module, bool) {
	modulesMu.RLock()
	defer modulesMu.RUnlock()
	m, ok := modules[name]
	return m, ok
}

// ---------------------------------------------------------------------------
// WAV parse / write (ported from your tools; format from the fmt chunk)
// ---------------------------------------------------------------------------

type wavInfo struct {
	sampleRate, dataLen uint32
	channels, bits      uint16
	dataStart           int
}

func parseWAV(b []byte) (*wavInfo, error) {
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("not a valid WAV file")
	}
	var info wavInfo
	foundFmt, foundData := false, false
	offset := 12
	for offset+8 <= len(b) {
		id := string(b[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(b[offset+4 : offset+8]))
		bodyStart := offset + 8
		if bodyStart+size > len(b) {
			size = len(b) - bodyStart
		}
		switch id {
		case "fmt ":
			if size >= 16 {
				info.channels = binary.LittleEndian.Uint16(b[bodyStart+2 : bodyStart+4])
				info.sampleRate = binary.LittleEndian.Uint32(b[bodyStart+4 : bodyStart+8])
				info.bits = binary.LittleEndian.Uint16(b[bodyStart+14 : bodyStart+16])
				foundFmt = true
			}
		case "data":
			info.dataStart = bodyStart
			info.dataLen = uint32(size)
			foundData = true
		}
		if foundFmt && foundData {
			break
		}
		offset = bodyStart + size
		if size%2 != 0 {
			offset++
		}
	}
	if !foundFmt || !foundData {
		return nil, errors.New("WAV missing fmt or data chunk")
	}
	if info.channels == 0 || info.sampleRate == 0 || info.bits == 0 {
		return nil, errors.New("WAV has invalid format fields")
	}
	return &info, nil
}

func pcmToWAV(s *PCMState) []byte {
	byteRate := s.SampleRate * uint32(s.Channels) * uint32(s.BitsPerSample) / 8
	blockAlign := s.Channels * s.BitsPerSample / 8
	dataSize := uint32(len(s.Data))

	buf := bytes.NewBuffer(make([]byte, 0, 44+len(s.Data)))
	tmp4 := make([]byte, 4)
	tmp2 := make([]byte, 2)

	buf.Write([]byte("RIFF"))
	binary.LittleEndian.PutUint32(tmp4, 36+dataSize)
	buf.Write(tmp4)
	buf.Write([]byte("WAVE"))

	buf.Write([]byte("fmt "))
	binary.LittleEndian.PutUint32(tmp4, 16)
	buf.Write(tmp4)
	binary.LittleEndian.PutUint16(tmp2, 1) // PCM
	buf.Write(tmp2)
	binary.LittleEndian.PutUint16(tmp2, s.Channels)
	buf.Write(tmp2)
	binary.LittleEndian.PutUint32(tmp4, s.SampleRate)
	buf.Write(tmp4)
	binary.LittleEndian.PutUint32(tmp4, byteRate)
	buf.Write(tmp4)
	binary.LittleEndian.PutUint16(tmp2, blockAlign)
	buf.Write(tmp2)
	binary.LittleEndian.PutUint16(tmp2, s.BitsPerSample)
	buf.Write(tmp2)

	buf.Write([]byte("data"))
	binary.LittleEndian.PutUint32(tmp4, dataSize)
	buf.Write(tmp4)
	buf.Write(s.Data)

	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// BMP write (ported from your wav2bmp; metadata smuggled into header fields)
// ---------------------------------------------------------------------------

func pcmToBMP(s *PCMState) []byte {
	pad := (3 - len(s.Data)%3) % 3
	padded := make([]byte, len(s.Data)+pad)
	copy(padded, s.Data)

	width, height := s.BMPDimensions()
	rowSize := width * 3
	usable := rowSize * height

	buf := bytes.NewBuffer(make([]byte, 0, 54+usable))
	tmp4 := make([]byte, 4)
	tmp2 := make([]byte, 2)

	// File header
	buf.WriteByte('B')
	buf.WriteByte('M')
	binary.LittleEndian.PutUint32(tmp4, uint32(54+usable)) // file size
	buf.Write(tmp4)
	binary.LittleEndian.PutUint32(tmp4, uint32(len(s.Data))) // PCM length → reserved
	buf.Write(tmp4)
	binary.LittleEndian.PutUint32(tmp4, 54) // pixel data offset
	buf.Write(tmp4)

	// Info header
	binary.LittleEndian.PutUint32(tmp4, 40)
	buf.Write(tmp4)
	binary.LittleEndian.PutUint32(tmp4, uint32(width))
	buf.Write(tmp4)
	binary.LittleEndian.PutUint32(tmp4, uint32(height))
	buf.Write(tmp4)
	binary.LittleEndian.PutUint16(tmp2, 1) // planes
	buf.Write(tmp2)
	binary.LittleEndian.PutUint16(tmp2, 24) // bits per pixel
	buf.Write(tmp2)
	binary.LittleEndian.PutUint32(tmp4, 0) // BI_RGB
	buf.Write(tmp4)
	binary.LittleEndian.PutUint32(tmp4, uint32(usable)) // image size
	buf.Write(tmp4)
	binary.LittleEndian.PutUint32(tmp4, 2835) // ppm x
	buf.Write(tmp4)
	binary.LittleEndian.PutUint32(tmp4, 2835) // ppm y
	buf.Write(tmp4)
	binary.LittleEndian.PutUint32(tmp4, s.SampleRate)                                     // sample rate → "colors used"
	buf.Write(tmp4)
	binary.LittleEndian.PutUint32(tmp4, uint32(s.Channels)<<16|uint32(s.BitsPerSample)) // → "important colors"
	buf.Write(tmp4)

	// Pixel data, bottom-up rows, no row padding by construction.
	srcIdx := 0
	for y := height - 1; y >= 0; y-- {
		end := srcIdx + rowSize
		if end > len(padded) {
			end = len(padded)
		}
		row := padded[srcIdx:end]
		if len(row) < rowSize {
			full := make([]byte, rowSize)
			copy(full, row)
			row = full
		}
		buf.Write(row)
		srcIdx += rowSize
	}

	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// BMP parse (ported from your bmp2wav)
// ---------------------------------------------------------------------------

func parseBMP(b []byte) (*PCMState, error) {
	if len(b) < 54 || b[0] != 'B' || b[1] != 'M' {
		return nil, errors.New("not a valid BMP file")
	}

	width := int(int32(binary.LittleEndian.Uint32(b[18:22])))
	height := int(int32(binary.LittleEndian.Uint32(b[22:26])))
	bpp := int(binary.LittleEndian.Uint16(b[28:30]))
	pixelOffset := int(binary.LittleEndian.Uint32(b[10:14]))

	if width <= 0 || height <= 0 || bpp <= 0 {
		return nil, errors.New("BMP has invalid dimensions")
	}
	if pixelOffset < 54 || pixelOffset >= len(b) {
		return nil, errors.New("BMP has invalid pixel offset")
	}

	// Smuggled metadata (written by pcmToBMP / your wav2bmp)
	originalPCMLen := binary.LittleEndian.Uint32(b[6:10])
	sampleRate := binary.LittleEndian.Uint32(b[46:50])
	formatField := binary.LittleEndian.Uint32(b[50:54])
	numChannels := uint16(formatField >> 16)
	bitsPerSample := uint16(formatField & 0xFFFF)

	// Only trust originalPCMLen if this is our smuggled metadata
	if sampleRate == 0 || formatField == 0 {
		originalPCMLen = 0
	}

	// Fallbacks for foreign BMPs
	if sampleRate == 0 {
		sampleRate = 44100
	}
	if numChannels == 0 {
		numChannels = 1
	}
	if bitsPerSample == 0 {
		bitsPerSample = 16
	}

	rowSize := width * bpp / 8
	if rowSize <= 0 {
		return nil, errors.New("BMP row size is zero")
	}
	rowPadding := (4 - rowSize%4) % 4
	paddedRowSize := rowSize + rowPadding

	pixelData := b[pixelOffset:]
	allocCap := rowSize * height
	if allocCap > len(pixelData) {
		allocCap = len(pixelData)
	}
	rawBytes := make([]byte, 0, allocCap)
	for y := 0; y < height; y++ {
		fileRow := (height - 1 - y) * paddedRowSize
		end := fileRow + rowSize
		if fileRow >= len(pixelData) {
			break
		}
		if end > len(pixelData) {
			end = len(pixelData)
		}
		rawBytes = append(rawBytes, pixelData[fileRow:end]...)
	}

	if originalPCMLen > 0 && int(originalPCMLen) <= len(rawBytes) {
		rawBytes = rawBytes[:originalPCMLen]
	}

	return &PCMState{
		Data:          rawBytes,
		SampleRate:    sampleRate,
		Channels:      numChannels,
		BitsPerSample: bitsPerSample,
	}, nil
}

// ---------------------------------------------------------------------------
// Waveform peaks (mono mixdown, min/max per bucket, normalized -1..1)
// ---------------------------------------------------------------------------

func computePeaks(s *PCMState, buckets int) ([]float64, []float64) {
	mins := make([]float64, buckets)
	maxs := make([]float64, buckets)
	for i := range mins {
		mins[i] = 0
		maxs[i] = 0
	}

	var frames int
	bytesPerFrame := int(s.Channels) * int(s.BitsPerSample) / 8
	if bytesPerFrame == 0 {
		return mins, maxs
	}
	frames = len(s.Data) / bytesPerFrame
	if frames == 0 {
		return mins, maxs
	}

	frameAt := func(i int) float64 {
		off := i * bytesPerFrame
		var sum float64
		for ch := 0; ch < int(s.Channels); ch++ {
			b := s.Data[off+ch*(int(s.BitsPerSample)/8):]
			sum += decodeSample(b, s.BitsPerSample)
		}
		return sum / float64(s.Channels)
	}

	perBucket := frames / buckets
	if perBucket < 1 {
		perBucket = 1
	}
	for bi := 0; bi < buckets; bi++ {
		start := bi * perBucket
		end := start + perBucket
		if bi == buckets-1 {
			end = frames // last bucket absorbs remainder
		}
		if start >= frames {
			break
		}
		mn, mx := 1.0, -1.0
		step := (end - start) / maxSamplesInBucket
		if step < 1 {
			step = 1
		}
		for f := start; f < end; f += step {
			v := frameAt(f)
			if v < mn {
				mn = v
			}
			if v > mx {
				mx = v
			}
		}
		if mn > mx { // empty bucket
			mn, mx = 0, 0
		}
		mins[bi] = mn
		maxs[bi] = mx
	}
	return mins, maxs
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

func jsonErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

type stateResponse struct {
	HasState      bool      `json:"hasState"`
	FileName      string    `json:"fileName"`
	Kind          string    `json:"kind"`
	SampleRate    uint32    `json:"sampleRate"`
	Channels      uint16    `json:"channels"`
	BitsPerSample uint16    `json:"bitsPerSample"`
	Bytes         int       `json:"bytes"`
	DurationSec   float64   `json:"durationSec"`
	ImageWidth    int        `json:"imageWidth"`
	ImageHeight   int        `json:"imageHeight"`
	CanUndo       bool      `json:"canUndo"`
	ChainLength   int       `json:"chainLength"`
	PeakMin       []float64 `json:"peakMin"`
	PeakMax       []float64 `json:"peakMax"`
}

func handleState(w http.ResponseWriter, r *http.Request) {
	sess.mu.Lock()
	resp := stateResponse{}
	if sess.hasState {
		s := &sess.current
		w, h := s.BMPDimensions()
		pmin, pmax := computePeaks(s, peakBuckets)
		resp = stateResponse{
			HasState:      true,
			FileName:      sess.fileName,
			Kind:          sess.kind,
			SampleRate:    s.SampleRate,
			Channels:      s.Channels,
			BitsPerSample: s.BitsPerSample,
			Bytes:         len(s.Data),
			DurationSec:   s.DurationSec(),
			ImageWidth:    w,
			ImageHeight:   h,
			CanUndo:       len(sess.history) > 0,
			ChainLength:   len(sess.chain),
			PeakMin:       pmin,
			PeakMax:       pmax,
		}
	}
	sess.mu.Unlock()
	jsonOK(w, resp)
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			r.MultipartForm.RemoveAll()
		}
	}()
	file, hdr, err := r.FormFile("file")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "missing file field")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxUpload+1))
	if err != nil || len(data) > maxUpload {
		jsonErr(w, http.StatusRequestEntityTooLarge, "file too large")
		return
	}

	var st *PCMState
	var kind string
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WAVE" {
		st, err = parseWAVtoState(data)
		kind = "wav"
	} else if len(data) >= 2 && data[0] == 'B' && data[1] == 'M' {
		st, err = parseBMP(data)
		kind = "bmp"
	} else {
		err = errors.New("unrecognized file type (need WAV or BMP)")
	}
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.base = *st
	sess.current = st.Clone()
	sess.history = nil
	sess.chain = nil
	sess.chainHistory = nil
	sess.fileName = hdr.Filename
	sess.kind = kind
	sess.hasState = true

	log.Printf("uploaded %s (%s): %d bytes PCM, %d Hz, %d-bit, %d ch",
		hdr.Filename, kind, len(st.Data), st.SampleRate, st.BitsPerSample, st.Channels)
	jsonOK(w, map[string]any{"ok": true, "kind": kind, "bytes": len(st.Data)})
}

func parseWAVtoState(b []byte) (*PCMState, error) {
	info, err := parseWAV(b)
	if err != nil {
		return nil, err
	}
	if info.dataStart+int(info.dataLen) > len(b) {
		return nil, errors.New("WAV data chunk exceeds file length")
	}
	return &PCMState{
		Data:          b[info.dataStart : info.dataStart+int(info.dataLen)],
		SampleRate:    info.sampleRate,
		Channels:      info.channels,
		BitsPerSample: info.bits,
	}, nil
}

func handleAudio(w http.ResponseWriter, r *http.Request) {
	sess.mu.Lock()
	if !sess.hasState {
		sess.mu.Unlock()
		jsonErr(w, http.StatusNotFound, "no state")
		return
	}
	wav := pcmToWAV(&sess.current)
	stem := sess.stemName()
	sess.mu.Unlock()

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", stem+".wav"))
	}
	http.ServeContent(w, r, stem+".wav", time.Time{}, bytes.NewReader(wav))
}

func handleImage(w http.ResponseWriter, r *http.Request) {
	sess.mu.Lock()
	if !sess.hasState {
		sess.mu.Unlock()
		jsonErr(w, http.StatusNotFound, "no state")
		return
	}
	bmp := pcmToBMP(&sess.current)
	stem := sess.stemName()
	sess.mu.Unlock()

	w.Header().Set("Content-Type", "image/bmp")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", stem+".bmp"))
	}
	http.ServeContent(w, r, stem+".bmp", time.Time{}, bytes.NewReader(bmp))
}

func handleExportWAV(w http.ResponseWriter, r *http.Request) {
	sess.mu.Lock()
	if !sess.hasState {
		sess.mu.Unlock()
		jsonErr(w, http.StatusNotFound, "no state")
		return
	}
	wav := pcmToWAV(&sess.current)
	stem := sess.stemName()
	sess.mu.Unlock()

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", stem+".wav"))
	http.ServeContent(w, r, stem+".wav", time.Time{}, bytes.NewReader(wav))
}

func handleExportBMP(w http.ResponseWriter, r *http.Request) {
	sess.mu.Lock()
	if !sess.hasState {
		sess.mu.Unlock()
		jsonErr(w, http.StatusNotFound, "no state")
		return
	}
	bmp := pcmToBMP(&sess.current)
	stem := sess.stemName()
	sess.mu.Unlock()

	w.Header().Set("Content-Type", "image/bmp")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", stem+".bmp"))
	http.ServeContent(w, r, stem+".bmp", time.Time{}, bytes.NewReader(bmp))
}

// pushHistory records current state for undo. Caller holds sess.mu.
func (s *Session) pushHistory() {
	if len(s.history) >= maxHistory {
		s.history = s.history[1:]
		if len(s.chainHistory) > 0 {
			s.chainHistory = s.chainHistory[1:]
		}
	}
	s.history = append(s.history, s.current.Clone())
	s.chainHistory = append(s.chainHistory, cloneChain(s.chain))
}

func handleUndo(w http.ResponseWriter, r *http.Request) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(sess.history) == 0 {
		jsonErr(w, http.StatusBadRequest, "nothing to undo")
		return
	}
	sess.current = sess.history[len(sess.history)-1]
	sess.history = sess.history[:len(sess.history)-1]
	if len(sess.chainHistory) > 0 {
		sess.chain = sess.chainHistory[len(sess.chainHistory)-1]
		sess.chainHistory = sess.chainHistory[:len(sess.chainHistory)-1]
	}
	jsonOK(w, map[string]any{"ok": true})
}

func handleRevert(w http.ResponseWriter, r *http.Request) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if !sess.hasState {
		jsonErr(w, http.StatusBadRequest, "nothing loaded")
		return
	}
	sess.current = sess.base.Clone()
	sess.history = nil
	sess.chain = nil
	sess.chainHistory = nil
	jsonOK(w, map[string]any{"ok": true})
}

func handleModulesList(w http.ResponseWriter, r *http.Request) {
	modulesMu.RLock()
	defer modulesMu.RUnlock()
	// PHASE 9: hidden modules (scan_keep) are dispatchable but never
	// listed — the palette stays clean.
	out := make([]*Module, 0, len(modules))
	for _, m := range modules {
		if m.Hidden {
			continue
		}
		out = append(out, m)
	}
	jsonOK(w, map[string]any{"modules": out})
}

// applyModuleLocked resolves a module, merges params with defaults, applies
// it to a copy of the current state, and commits with one undo step on
// success. Caller must hold sess.mu. Shared by /api/module/apply and
// /api/scan/keep so both flow through identical pipeline discipline.
// (PHASE 9)
func applyModuleLocked(moduleName string, overrides map[string]float64) error {
	mod, ok := getModule(moduleName)
	if !ok {
		return fmt.Errorf("unknown module: %s", moduleName)
	}
	if !sess.hasState {
		return errors.New("nothing loaded")
	}

	// Fill in defaults for unspecified params.
	params := map[string]float64{}
	for _, p := range mod.Params {
		params[p.Name] = p.Def
	}
	for k, v := range overrides {
		if _, ok := params[k]; ok {
			params[k] = v
		}
	}

	// Apply on a copy; commit only on success, so failed modules
	// never corrupt the session.
	work := sess.current.Clone()
	if err := mod.Apply(&work, params); err != nil {
		return err
	}
	sess.pushHistory()
	sess.current = work
	if !mod.Hidden && moduleName != "frame_probe" {
		pCopy := make(map[string]float64, len(params))
		for k, v := range params {
			pCopy[k] = v
		}
		sess.chain = append(sess.chain, PresetStep{Module: moduleName, Params: pCopy})
	}
	return nil
}

// ---------------------------------------------------------------------------
// PHASE 10.2: Preset export / import (effect chains as JSON)
// ---------------------------------------------------------------------------

func handlePresetGet(w http.ResponseWriter, r *http.Request) {
	sess.mu.Lock()
	chain := cloneChain(sess.chain)
	if chain == nil {
		chain = []PresetStep{}
	}
	stem := sess.stemName()
	sess.mu.Unlock()

	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", stem+"-preset.json"))
	}
	jsonOK(w, map[string]any{
		"name":  stem,
		"steps": chain,
	})
}

func handlePresetApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string       `json:"name"`
		Steps []PresetStep `json:"steps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	if len(req.Steps) == 0 {
		jsonErr(w, http.StatusBadRequest, "preset contains no steps")
		return
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	if !sess.hasState {
		jsonErr(w, http.StatusConflict, "no signal loaded — upload a file first")
		return
	}

	// Validate and apply all steps onto a single clone of the current state.
	// If any step fails, sess.current remains untouched.
	work := sess.current.Clone()
	resolvedSteps := make([]PresetStep, 0, len(req.Steps))
	for _, st := range req.Steps {
		mod, ok := getModule(st.Module)
		if !ok {
			jsonErr(w, http.StatusNotFound, "unknown module: "+st.Module)
			return
		}
		params := map[string]float64{}
		for _, p := range mod.Params {
			params[p.Name] = p.Def
		}
		for k, v := range st.Params {
			if _, ok := params[k]; ok {
				params[k] = v
			}
		}
		if err := mod.Apply(&work, params); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !mod.Hidden && st.Module != "frame_probe" {
			resolvedSteps = append(resolvedSteps, PresetStep{Module: st.Module, Params: params})
		}
	}

	sess.pushHistory()
	sess.current = work
	sess.chain = append(sess.chain, resolvedSteps...)
	jsonOK(w, map[string]any{"ok": true, "applied": len(req.Steps)})
}

func handleModuleApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Module string             `json:"module"`
		Params map[string]float64 `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad JSON body")
		return
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	if err := applyModuleLocked(req.Module, req.Params); err != nil {
		if _, known := getModule(req.Module); !known {
			jsonErr(w, http.StatusNotFound, err.Error())
		} else {
			jsonErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// PHASE 9: Scanner HTTP handlers.
// Engine lives in scan.go (ScanManager, scan_keep module, staged finding).
// ---------------------------------------------------------------------------

func handleScanStart(w http.ResponseWriter, r *http.Request) {
	scanMgr.Start()
	jsonOK(w, map[string]any{"ok": true, "running": true})
}

func handleScanStop(w http.ResponseWriter, r *http.Request) {
	scanMgr.Stop()
	st := scanMgr.Status()
	jsonOK(w, map[string]any{
		"ok": true, "running": st.Running, "segments": st.Segments,
		"best_score": st.Best, "elapsed_ms": st.Elapsed.Milliseconds(),
		"source": st.SrcID, "source_name": st.SrcName,
	})
}

func handleScanStatus(w http.ResponseWriter, r *http.Request) {
	st := scanMgr.Status()
	jsonOK(w, map[string]any{
		"running":          st.Running,
		"segments":         st.Segments,
		"best_score":       st.Best,
		"has_finding":      st.HasFinding,
		"elapsed_ms":       st.Elapsed.Milliseconds(),
		"segment_samples": scanSegmentSamples,
		"source":           st.SrcID,
		"source_name":      st.SrcName,
	})
}

func handleScanKeep(w http.ResponseWriter, r *http.Request) {
	sess.mu.Lock()
	if !sess.hasState {
		sess.mu.Unlock()
		jsonErr(w, http.StatusConflict, "no signal loaded — upload a file first")
		return
	}
	sess.mu.Unlock()

	// Consume the best finding (atomically clears it for the next hunt).
	raw := scanMgr.TakeFinding()
	if raw == nil {
		jsonErr(w, http.StatusConflict, "no finding in the scanner")
		return
	}
	stageScanFinding(raw)

	sess.mu.Lock()
	err := applyModuleLocked("scan_keep", nil)
	sess.mu.Unlock()

	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/upload", handleUpload)
	mux.HandleFunc("GET /api/state", handleState)
	mux.HandleFunc("GET /audio/current.wav", handleAudio)
	mux.HandleFunc("GET /image/current.bmp", handleImage)
	mux.HandleFunc("POST /api/undo", handleUndo)
	mux.HandleFunc("POST /api/revert", handleRevert)
	mux.HandleFunc("GET /api/modules", handleModulesList)
	mux.HandleFunc("POST /api/module/apply", handleModuleApply)

	// PHASE 9: scanner endpoints.
	mux.HandleFunc("POST /api/scan/start", handleScanStart)
	mux.HandleFunc("POST /api/scan/stop", handleScanStop)
	mux.HandleFunc("GET /api/scan/status", handleScanStatus)
	mux.HandleFunc("POST /api/scan/keep", handleScanKeep)
	
	mux.HandleFunc("GET /api/scan/sources", handleScanSourcesList)
	mux.HandleFunc("POST /api/scan/sources", handleScanSourceUpload)
	mux.HandleFunc("POST /api/scan/source/select", handleScanSourceSelect)

	// PHASE 10.2: export & preset endpoints.
	mux.HandleFunc("GET /api/export/wav", handleExportWAV)
	mux.HandleFunc("GET /api/export/bmp", handleExportBMP)
	mux.HandleFunc("GET /api/preset", handlePresetGet)
	mux.HandleFunc("POST /api/preset/apply", handlePresetApply)

	// Static web UI embedded directly into the binary (100% self-contained)
	webContent, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		log.Fatalf("failed to load embedded web assets: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(webContent)))

	fmt.Println("Uncharted Spectra — core engine")
	fmt.Printf("listening on http://%s\n", listenAddr)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}
