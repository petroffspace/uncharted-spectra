package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestModuleRegistry(t *testing.T) {
	expected := []string{
		"spatial_gain",
		"spatial_filter",
		"spatial_distortion",
		"spatial_delay",
		"spatial_reverb",
		"spatial_bitcrush",
		"temporal_adjust",
		"temporal_blur",
		"temporal_fill",
		"terrain_apply",
		"frame_probe",
		"scan_keep",
	}

	for _, name := range expected {
		if _, ok := getModule(name); !ok {
			t.Errorf("expected module %s to be registered", name)
		}
	}
}

func TestFrameProbeLossless(t *testing.T) {
	data, err := os.ReadFile("sample.wav")
	if err != nil {
		t.Fatalf("failed to read sample.wav: %v", err)
	}

	st, err := parseWAVtoState(data)
	if err != nil {
		t.Fatalf("failed to parse WAV: %v", err)
	}

	// 1. probeFrames directly
	if err := probeFrames(st); err != nil {
		t.Fatalf("probeFrames failed: %v", err)
	}

	// 2. SpatialFrame round-trip
	sf, err := NewSpatialFrame(st)
	if err != nil {
		t.Fatalf("NewSpatialFrame failed: %v", err)
	}
	pcmFromSF := sf.ToPCM()
	if !bytes.Equal(pcmFromSF, st.Data) {
		t.Fatalf("SpatialFrame ToPCM mismatch: got %d bytes, want %d bytes", len(pcmFromSF), len(st.Data))
	}

	// 3. TemporalFrame round-trip
	tf, err := NewTemporalFrame(st)
	if err != nil {
		t.Fatalf("NewTemporalFrame failed: %v", err)
	}
	pcmFromTF := tf.ToPCM()
	if !bytes.Equal(pcmFromTF, st.Data) {
		t.Fatalf("TemporalFrame ToPCM mismatch: got %d bytes, want %d bytes", len(pcmFromTF), len(st.Data))
	}
}

func TestNullContracts(t *testing.T) {
	data, err := os.ReadFile("sample.wav")
	if err != nil {
		t.Fatalf("failed to read sample.wav: %v", err)
	}
	baseState, err := parseWAVtoState(data)
	if err != nil {
		t.Fatalf("failed to parse WAV: %v", err)
	}

	// Test modules with mix = 0
	mixZeroModules := []string{
		"spatial_gain",
		"spatial_filter",
		"spatial_distortion",
		"spatial_delay",
		"spatial_bitcrush",
		"temporal_adjust",
		"temporal_blur",
		"temporal_fill",
	}

	for _, name := range mixZeroModules {
		mod, ok := getModule(name)
		if !ok {
			t.Fatalf("module %s not found", name)
		}
		testState := baseState.Clone()
		params := map[string]float64{"mix": 0}
		for _, p := range mod.Params {
			if p.Name != "mix" {
				params[p.Name] = p.Def
			}
		}

		if err := mod.Apply(&testState, params); err != nil {
			t.Errorf("module %s failed: %v", name, err)
		}
		if !bytes.Equal(testState.Data, baseState.Data) {
			t.Errorf("null contract violation on module %s (mix=0 modified bytes)", name)
		}
	}

	// Test spatial_reverb with wet = 0, gain = 1
	{
		mod, _ := getModule("spatial_reverb")
		testState := baseState.Clone()
		params := map[string]float64{
			"size": 0.5,
			"damp": 0.5,
			"wet":  0.0,
			"gain": 1.0,
		}
		if err := mod.Apply(&testState, params); err != nil {
			t.Errorf("reverb failed: %v", err)
		}
		if !bytes.Equal(testState.Data, baseState.Data) {
			t.Errorf("null contract violation on spatial_reverb (wet=0, gain=1 modified bytes)")
		}
	}

	// Test terrain_apply with shift = 0, randomize = 0
	{
		mod, _ := getModule("terrain_apply")
		testState := baseState.Clone()
		params := map[string]float64{
			"shift_x_px":    0,
			"shift_y_px":    0,
			"randomize_pct": 0,
		}
		if err := mod.Apply(&testState, params); err != nil {
			t.Errorf("terrain_apply failed: %v", err)
		}
		if !bytes.Equal(testState.Data, baseState.Data) {
			t.Errorf("null contract violation on terrain_apply (shifts=0 modified bytes)")
		}
	}

	// Test temporal_fill region with start == end
	{
		mod, _ := getModule("temporal_fill")
		testState := baseState.Clone()
		params := map[string]float64{
			"mode":  0,
			"start": 0.2,
			"end":   0.2,
			"mix":   100,
		}
		if err := mod.Apply(&testState, params); err != nil {
			t.Errorf("temporal_fill start==end failed: %v", err)
		}
		if !bytes.Equal(testState.Data, baseState.Data) {
			t.Errorf("null contract violation on temporal_fill start==end")
		}
	}

	// Test temporal_fill salt with density == 0
	{
		mod, _ := getModule("temporal_fill")
		testState := baseState.Clone()
		params := map[string]float64{
			"mode":    1,
			"density": 0,
			"mix":     100,
		}
		if err := mod.Apply(&testState, params); err != nil {
			t.Errorf("temporal_fill density=0 failed: %v", err)
		}
		if !bytes.Equal(testState.Data, baseState.Data) {
			t.Errorf("null contract violation on temporal_fill density=0")
		}
	}
}

func TestFoldbackSymmetry(t *testing.T) {
	testPoints := []float64{0.1, 0.5, 0.9, 1.0, 1.2, 1.5, 2.0, 2.5, 3.0}
	for _, pt := range testPoints {
		pos := foldIntoUnit(pt)
		neg := foldIntoUnit(-pt)
		if math.Abs(pos+neg) > 1e-12 {
			t.Errorf("foldIntoUnit asymmetry: f(%f) = %f, f(-%f) = %f (sum = %f)", pt, pos, pt, neg, pos+neg)
		}
	}
}

func TestBandpassKernelSymmetry(t *testing.T) {
	for radius := 1; radius <= 10; radius++ {
		k := derivedKernel(2, radius) // 2 = bandpass
		n := len(k)
		for i := 0; i < n/2; i++ {
			left := k[i]
			right := k[n-1-i]
			if math.Abs(left-right) > 1e-12 {
				t.Errorf("bandpass kernel asymmetric at radius %d, index %d: left=%f, right=%f", radius, i, left, right)
			}
		}
		// Center should be peak
		centerIdx := n / 2
		for i := range k {
			if k[i] > k[centerIdx] {
				t.Errorf("bandpass kernel peak not at center: index %d has %f > center %f", i, k[i], k[centerIdx])
			}
		}
	}
}

func TestBoxBlurLargeRadius(t *testing.T) {
	// Signal of constant 10.0
	src := []float64{10.0, 10.0, 10.0, 10.0}
	dst := make([]float64, len(src))
	boxBlur1D(src, dst, 100) // radius >> length
	for i, v := range dst {
		if math.Abs(v-10.0) > 1e-9 {
			t.Errorf("boxBlur1D squashed constant signal: dst[%d] = %f, want 10.0", i, v)
		}
	}
}

func TestComputePeaksMultiDepth(t *testing.T) {
	// Test 16-bit
	pcm16 := make([]byte, 4000)
	st16 := &PCMState{
		Data:          pcm16,
		SampleRate:    44100,
		Channels:      1,
		BitsPerSample: 16,
	}
	min16, max16 := computePeaks(st16, 10)
	if len(min16) != 10 || len(max16) != 10 {
		t.Fatalf("unexpected peak array lengths")
	}

	// Test 24-bit
	pcm24 := make([]byte, 6000)
	st24 := &PCMState{
		Data:          pcm24,
		SampleRate:    44100,
		Channels:      1,
		BitsPerSample: 24,
	}
	min24, max24 := computePeaks(st24, 10)
	if len(min24) != 10 || len(max24) != 10 {
		t.Fatalf("unexpected peak array lengths")
	}

	// Test 32-bit
	pcm32 := make([]byte, 8000)
	st32 := &PCMState{
		Data:          pcm32,
		SampleRate:    44100,
		Channels:      1,
		BitsPerSample: 32,
	}
	min32, max32 := computePeaks(st32, 10)
	if len(min32) != 10 || len(max32) != 10 {
		t.Fatalf("unexpected peak array lengths")
	}
}

func TestScanSourceAlignment(t *testing.T) {
	// Create sample data of known 16-bit samples
	data := make([]byte, 65536)
	for i := 0; i < len(data)/2; i++ {
		data[2*i] = byte(i & 0xFF)
		data[2*i+1] = byte((i >> 8) & 0xFF)
	}

	src := &bytesSource{id: "test", name: "test", data: data}
	seg := make([]byte, 4096)
	for trial := 0; trial < 100; trial++ {
		if !src.Next(seg) {
			t.Fatalf("Next returned false")
		}
		// First sample in seg must match an actual sample (not split across bytes)
		v := int16(uint16(seg[0]) | uint16(seg[1])<<8)
		// Check that seg[0] and seg[1] correspond to an index
		if int(v)*2 >= len(data) || v < 0 {
			t.Errorf("misaligned sample extracted: %d", v)
		}
	}
}

func TestScanKeepNoStateReturns409(t *testing.T) {
	// Ensure sess has no state
	sess.mu.Lock()
	sess.hasState = false
	sess.mu.Unlock()

	req := httptest.NewRequest("POST", "/api/scan/keep", nil)
	w := httptest.NewRecorder()
	handleScanKeep(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict when nothing loaded, got %d", w.Code)
	}
}

func TestExportAndPresets(t *testing.T) {
	// Set up a small synthetic session
	pcm := make([]byte, 4800)
	for i := range pcm {
		pcm[i] = byte(i & 0xFF)
	}
	st := PCMState{
		Data:          pcm,
		SampleRate:    44100,
		Channels:      1,
		BitsPerSample: 16,
	}

	sess.mu.Lock()
	sess.base = st.Clone()
	sess.current = st.Clone()
	sess.history = nil
	sess.chain = nil
	sess.chainHistory = nil
	sess.fileName = "synth.wav"
	sess.kind = "wav"
	sess.hasState = true
	sess.mu.Unlock()

	// 1. Export WAV checks Content-Disposition
	{
		req := httptest.NewRequest("GET", "/api/export/wav", nil)
		w := httptest.NewRecorder()
		handleExportWAV(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("handleExportWAV status = %d", w.Code)
		}
		cd := w.Header().Get("Content-Disposition")
		if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "synth.wav") {
			t.Errorf("unexpected Content-Disposition for WAV export: %q", cd)
		}
	}

	// 2. Export BMP checks Content-Disposition
	{
		req := httptest.NewRequest("GET", "/api/export/bmp", nil)
		w := httptest.NewRecorder()
		handleExportBMP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("handleExportBMP status = %d", w.Code)
		}
		cd := w.Header().Get("Content-Disposition")
		if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "synth.bmp") {
			t.Errorf("unexpected Content-Disposition for BMP export: %q", cd)
		}
	}

	// 3. Apply a module and verify preset chain records it
	sess.mu.Lock()
	if err := applyModuleLocked("spatial_gain", map[string]float64{"gain_db": 6, "mix": 100}); err != nil {
		sess.mu.Unlock()
		t.Fatalf("applyModuleLocked failed: %v", err)
	}
	sess.mu.Unlock()

	var exported struct {
		Name  string       `json:"name"`
		Steps []PresetStep `json:"steps"`
	}
	{
		req := httptest.NewRequest("GET", "/api/preset?download=1", nil)
		w := httptest.NewRecorder()
		handlePresetGet(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("handlePresetGet status = %d", w.Code)
		}
		if err := json.NewDecoder(w.Body).Decode(&exported); err != nil {
			t.Fatalf("failed to decode preset: %v", err)
		}
		if len(exported.Steps) != 1 || exported.Steps[0].Module != "spatial_gain" {
			t.Fatalf("unexpected exported preset steps: %+v", exported.Steps)
		}
	}

	// Save the mutated state after spatial_gain
	sess.mu.Lock()
	mutatedBytes := append([]byte(nil), sess.current.Data...)
	sess.mu.Unlock()

	// 4. Undo should pop both state and preset chain
	{
		req := httptest.NewRequest("POST", "/api/undo", nil)
		w := httptest.NewRecorder()
		handleUndo(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("handleUndo status = %d", w.Code)
		}
		sess.mu.Lock()
		chainLen := len(sess.chain)
		restored := bytes.Equal(sess.current.Data, pcm)
		sess.mu.Unlock()
		if chainLen != 0 {
			t.Errorf("expected chain length 0 after undo, got %d", chainLen)
		}
		if !restored {
			t.Errorf("expected state bytes restored to base after undo")
		}
	}

	// 5. Re-apply the exported preset via POST /api/preset/apply
	{
		body, _ := json.Marshal(exported)
		req := httptest.NewRequest("POST", "/api/preset/apply", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handlePresetApply(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("handlePresetApply status = %d: %s", w.Code, w.Body.String())
		}
		sess.mu.Lock()
		reappliedEqual := bytes.Equal(sess.current.Data, mutatedBytes)
		chainLen := len(sess.chain)
		sess.mu.Unlock()
		if !reappliedEqual {
			t.Errorf("re-applied preset did not reproduce exact mutated bytes")
		}
		if chainLen != 1 {
			t.Errorf("expected chain length 1 after preset apply, got %d", chainLen)
		}
	}
}
