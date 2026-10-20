package main

// scan.go — the Scanner: background entropy scanner with structure index
// and pluggable scan sources. (Phase 9.0 + 10.0)
//
// A goroutine pulls candidate segments from the current ScanSource and
// scores each with a structure index composed of three independent
// noise-detectors:
//
//   1. spectral flatness  — geometric/arithmetic mean ratio of the power
//                           spectrum (Hann-windowed, 4096-pt FFT).
//   2. repetition         — peak normalized autocorrelation over a spread
//                           of lags.
//   3. peakiness          — strongest spectral peak vs spectral mean.
//
// Sources (Phase 10.0):
//   urandom  — /dev/urandom via the shared entropy source (default)
//   state    — frozen snapshot of the session PCM at selection time
//   file-N   — uploaded WAV/BMP/raw files, parsed to raw PCM bytes,
//              segments drawn at uniformly random offsets (wrap-tiling
//              for files shorter than one segment)
//
// Endpoints owned by this file:
//   GET  /api/scan/sources        — list available sources
//   POST /api/scan/sources        — upload a new source file (multipart "file")
//   POST /api/scan/source/select  — {source: id} hot-swaps the active source
//
// Endpoints living in main.go (Start/Stop/Status/Keep) delegate to the
// manager; see handleScan* there.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sync"
	"time"
)

// ---------- tuning ----------

const (
	scanSegmentSamples = 16384 // samples per candidate (32 KB)
	scanFFTSize        = 4096  // analysis window for the structure index
	scanPause          = 3 * time.Millisecond
)

// ---------- structure index (unchanged from 9.0) ----------

// fft performs an in-place iterative radix-2 Cooley–Tukey FFT on re/im.
func fft(re, im []float64) {
	n := len(re)
	if n&(n-1) != 0 {
		return
	}
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j |= bit
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		ang := -2 * math.Pi / float64(length)
		wr, wi := math.Cos(ang), math.Sin(ang)
		half := length / 2
		for i := 0; i < n; i += length {
			cr, ci := 1.0, 0.0
			for j := 0; j < half; j++ {
				ur, ui := re[i+j], im[i+j]
				xr, xi := re[i+j+half], im[i+j+half]
				vr := xr*cr - xi*ci
				vi := xr*ci + xi*cr
				re[i+j], im[i+j] = ur+vr, ui+vi
				re[i+j+half], im[i+j+half] = ur-vr, ui-vi
				cr, ci = cr*wr-ci*wi, cr*wi+ci*wr
			}
		}
	}
}

// structureIndex scores a mono sample block [−1,1] on 0..100.
func structureIndex(samples []float64) float64 {
	n := len(samples)
	if n < scanFFTSize {
		return 0
	}

	re := make([]float64, scanFFTSize)
	im := make([]float64, scanFFTSize)
	for i := 0; i < scanFFTSize; i++ {
		w := 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(scanFFTSize-1)))
		re[i] = samples[i] * w
	}
	fft(re, im)

	bins := scanFFTSize / 2
	logSum, linSum, maxP := 0.0, 0.0, 0.0
	for b := 1; b <= bins; b++ {
		p := re[b]*re[b] + im[b]*im[b]
		linSum += p
		if p > maxP {
			maxP = p
		}
		if p > 1e-20 {
			logSum += math.Log(p)
		} else {
			logSum += math.Log(1e-20)
		}
	}
	meanP := linSum / float64(bins)
	flatness := 1.0
	if meanP > 1e-20 {
		flatness = math.Exp(logSum/float64(bins)) / meanP
	}
	peakiness := 0.0
	if meanP > 1e-20 {
		peakiness = math.Min(maxP/meanP/50.0, 1.0)
	}

	bestRep := 0.0
	for lag := 32; lag <= 1024; lag += 16 {
		var corr, en1, en2 float64
		for i := 0; i+lag < scanFFTSize; i++ {
			corr += samples[i] * samples[i+lag]
			en1 += samples[i] * samples[i]
			en2 += samples[i+lag] * samples[i+lag]
		}
		norm := math.Sqrt(en1*en2)
		if norm > 1e-9 {
			r := math.Abs(corr) / norm
			if r > bestRep {
				bestRep = r
			}
		}
	}
	rep := math.Min(bestRep/0.4, 1.0)

	score := 100 * (0.45*(1-flatness) + 0.35*rep + 0.20*peakiness)
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return score
}

// bytesToMonoSamples interprets raw bytes as little-endian int16 mono.
func bytesToMonoSamples(raw []byte) []float64 {
	cnt := len(raw) / 2
	out := make([]float64, cnt)
	for i := 0; i < cnt; i++ {
		v := int16(uint16(raw[2*i]) | uint16(raw[2*i+1])<<8)
		out[i] = float64(v) / 32768.0
	}
	return out
}

// ---------- scan sources (Phase 10.0) ----------

// ScanSource produces candidate segments on demand. Implementations must
// be safe for concurrent Next() calls from the scan loop while a handler
// swaps the active source — in practice sources are immutable after
// creation, so hot-swapping is just a pointer change.
type ScanSource interface {
	ID() string
	Name() string
	Size() int // 0 = unbounded/unknown
	// Next fills seg (length scanSegmentSamples*2) with candidate bytes.
	// Returns false only if the source cannot produce anything.
	Next(seg []byte) bool
}

// randUint32 draws one uniformly random uint32 from the shared entropy
// source — used for file offset selection.
func randUint32() uint32 {
	var b [4]byte
	randomBytes(b[:])
	return binary.LittleEndian.Uint32(b[:])
}

// urandomSource — the original frontier.
type urandomSource struct{}

func (urandomSource) ID() string   { return "urandom" }
func (urandomSource) Name() string { return "/dev/urandom" }
func (urandomSource) Size() int   { return 0 }
func (urandomSource) Next(seg []byte) bool {
	randomBytes(seg)
	return true
}

// bytesSource — a frozen block of PCM bytes (uploaded file or state
// snapshot). Segments are drawn at uniformly random offsets; files
// shorter than one segment are wrap-tiled.
type bytesSource struct {
	id   string
	name string
	data []byte
}

func (s *bytesSource) ID() string   { return s.id }
func (s *bytesSource) Name() string { return s.name }
func (s *bytesSource) Size() int    { return len(s.data) }
func (s *bytesSource) Next(seg []byte) bool {
	n := len(s.data)
	if n == 0 {
		return false
	}
	if n <= len(seg) {
		step := n - (n % 2)
		if step == 0 {
			step = n
		}
		for off := 0; off < len(seg); off += step {
			copy(seg[off:], s.data[:step])
		}
		return true
	}
	maxOff := n - len(seg)
	off := int(randUint32() % uint32(maxOff+1))
	off -= off % 2 // Align to 16-bit PCM sample boundary
	copy(seg, s.data[off:off+len(seg)])
	return true
}

// newStateSource freezes the current session PCM as a scan source.
func newStateSource() (ScanSource, error) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if !sess.hasState {
		return nil, errors.New("nothing loaded — upload a file first")
	}
	data := make([]byte, len(sess.current.Data))
	copy(data, sess.current.Data)
	return &bytesSource{id: "state", name: "current state (snapshot)", data: data}, nil
}

// ---------- source registry ----------

var srcReg = struct {
	mu     sync.Mutex
	order  []ScanSource
	nextID int
}{order: []ScanSource{urandomSource{}}}

// registerSource adds or replaces (by ID). Returns the stored source.
func registerSource(s ScanSource) ScanSource {
	srcReg.mu.Lock()
	defer srcReg.mu.Unlock()
	for i, existing := range srcReg.order {
		if existing.ID() == s.ID() {
			srcReg.order[i] = s
			return s
		}
	}
	srcReg.order = append(srcReg.order, s)
	return s
}

func lookupSource(id string) (ScanSource, bool) {
	srcReg.mu.Lock()
	defer srcReg.mu.Unlock()
	for _, s := range srcReg.order {
		if s.ID() == id {
			return s, true
		}
	}
	return nil, false
}

// newFileSource parses uploaded bytes (WAV, BMP, or raw PCM) and
// registers it under a fresh file-N id.
func newFileSource(name string, data []byte) (ScanSource, error) {
	var pcm []byte
	label := name
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WAVE" {
		st, err := parseWAVtoState(data)
		if err != nil {
			return nil, err
		}
		pcm = st.Data
	} else if len(data) >= 2 && data[0] == 'B' && data[1] == 'M' {
		st, err := parseBMP(data)
		if err != nil {
			return nil, err
		}
		pcm = st.Data
	} else if len(data) > 0 {
		pcm = data // raw bytes, interpreted as int16 LE mono
	} else {
		return nil, errors.New("empty file")
	}
	if len(pcm)%2 != 0 {
		pcm = pcm[:len(pcm)-1]
	}
	if len(pcm) < 2 {
		return nil, errors.New("file yields less than one sample")
	}

	srcReg.mu.Lock()
	srcReg.nextID++
	id := fmt.Sprintf("file-%d", srcReg.nextID)
	srcReg.mu.Unlock()

	s := &bytesSource{id: id, name: label, data: pcm}
	registerSource(s)
	return s, nil
}

// ---------- scan manager ----------

// scanStatusInfo is a snapshot of scanner state for status/stop replies.
type scanStatusInfo struct {
	Running    bool
	Segments   uint64
	Best       float64
	HasFinding bool
	Elapsed    time.Duration
	SrcID      string
	SrcName    string
}

type ScanManager struct {
	mu        sync.Mutex
	running   bool
	source    ScanSource // nil → urandom
	stopCh    chan struct{}
	segments  uint64
	bestScore float64
	bestRaw   []byte
	started   time.Time
}

var scanMgr = &ScanManager{}

// curSource returns the active source, defaulting to urandom.
func (m *ScanManager) curSource() ScanSource {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.source == nil {
		return urandomSource{}
	}
	return m.source
}

// Start begins/resumes scanning.
func (m *ScanManager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return
	}
	m.running = true
	m.stopCh = make(chan struct{})
	m.started = time.Now()
	go m.loop()
}

// Stop halts the goroutine. Finding and stats are retained.
func (m *ScanManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running {
		return
	}
	m.running = false
	close(m.stopCh)
}

// SetSource hot-swaps the active source (safe while running: the loop
// re-reads the pointer each iteration).
func (m *ScanManager) SetSource(src ScanSource) {
	m.mu.Lock()
	m.source = src
	m.mu.Unlock()
}

func (m *ScanManager) loop() {
	segBytes := scanSegmentSamples * 2
	for {
		select {
		case <-m.stopCh:
			return
		default:
		}
		src := m.curSource()
		raw := make([]byte, segBytes)
		ok := src.Next(raw)
		if ok {
			sc := structureIndex(bytesToMonoSamples(raw))
			m.mu.Lock()
			m.segments++
			if sc > m.bestScore {
				m.bestScore = sc
				m.bestRaw = raw
			}
			m.mu.Unlock()
		} else {
			// source exhausted/empty — avoid a hot spin
			time.Sleep(scanPause * 10)
		}
		time.Sleep(scanPause)
	}
}

// Status snapshots current scan state.
func (m *ScanManager) Status() scanStatusInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := time.Duration(0)
	if m.running {
		e = time.Since(m.started)
	}
	src := m.source
	id, name := "urandom", "/dev/urandom"
	if src != nil {
		id, name = src.ID(), src.Name()
	}
	return scanStatusInfo{
		Running:    m.running,
		Segments:   m.segments,
		Best:       m.bestScore,
		HasFinding: m.bestRaw != nil,
		Elapsed:    e,
		SrcID:      id,
		SrcName:    name,
	}
}

// TakeFinding removes and returns the best raw segment (nil if none).
func (m *ScanManager) TakeFinding() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bestRaw == nil {
		return nil
	}
	out := m.bestRaw
	m.bestRaw = nil
	m.bestScore = 0
	return out
}

// ---------- pending-finding handoff (unchanged from 9.0) ----------

var keepMu sync.Mutex
var pendingFinding []byte

func stageScanFinding(raw []byte) {
	keepMu.Lock()
	pendingFinding = raw
	keepMu.Unlock()
}

func takePendingFinding() []byte {
	keepMu.Lock()
	defer keepMu.Unlock()
	out := pendingFinding
	pendingFinding = nil
	return out
}

func applyScanKeeping(s *PCMState, _ map[string]float64) error {
	raw := takePendingFinding()
	if raw == nil {
		return fmt.Errorf("no scan finding staged")
	}
	samples := bytesToMonoSamples(raw)
	return applyTemporal(s, func(f *TemporalFrame) error {
		for c := 0; c < f.Channels; c++ {
			ch := f.Ch[c]
			for i := range ch {
				ch[i] = samples[i%len(samples)]
			}
		}
		return nil
	})
}

// ---------- HTTP handlers (sources) ----------

func handleScanSourcesList(w http.ResponseWriter, r *http.Request) {
	type srcRow struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Bytes int    `json:"bytes"`
	}
	out := []srcRow{}
	srcReg.mu.Lock()
	for _, s := range srcReg.order {
		out = append(out, srcRow{ID: s.ID(), Name: s.Name(), Bytes: s.Size()})
	}
	srcReg.mu.Unlock()

	// "state" is offered when a session exists; resolved lazily on select.
	sess.mu.Lock()
	hasState := sess.hasState
	sess.mu.Unlock()
	if hasState {
		out = append(out, srcRow{ID: "state", Name: "current state (snapshot)", Bytes: 0})
	}

	jsonOK(w, map[string]any{"sources": out})
}

func handleScanSourceUpload(w http.ResponseWriter, r *http.Request) {
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

	src, err := newFileSource(hdr.Filename, data)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("scan source added: %s (%s) — %d bytes PCM", src.ID(), src.Name(), src.Size())

	// Newly uploaded sources become the active one — least surprise.
	scanMgr.SetSource(src)
	jsonOK(w, map[string]any{"ok": true, "id": src.ID(), "name": src.Name()})
}

func handleScanSourceSelect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	var src ScanSource
	if req.Source == "state" {
		s, err := newStateSource()
		if err != nil {
			jsonErr(w, http.StatusConflict, err.Error())
			return
		}
		src = s
	} else {
		s, ok := lookupSource(req.Source)
		if !ok {
			jsonErr(w, http.StatusNotFound, "unknown source: "+req.Source)
			return
		}
		src = s
	}
	scanMgr.SetSource(src)
	jsonOK(w, map[string]any{"ok": true, "id": src.ID(), "name": src.Name()})
}

func init() {
	RegisterModule(&Module{
		Name:     "scan_keep",
		Category: "tool",
		Hidden:   true,
		Label:    "Scanner Keep (internal)",
		Params:   []ParamSpec{},
		Apply:    applyScanKeeping,
	})
}
