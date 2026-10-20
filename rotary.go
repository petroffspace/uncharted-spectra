package main

// rotary.go — terrain controls as a server module ("terrain_apply").
//
// Semantics (mirror of the client-side preview in web/app.js):
//   shift_x > 0  → image content moves RIGHT, fresh /dev/urandom column
//                  fills the vacated LEFT edge
//   shift_x < 0  → content moves LEFT, fresh column on the RIGHT edge
//   shift_y > 0  → content moves DOWN, fresh rows enter at the TOP
//   shift_y < 0  → content moves UP, fresh rows enter at the BOTTOM
//   randomize    → replace that percentage of pixels with fresh entropy,
//                  pixels chosen UNIFORMLY at random (partial Fisher-Yates)
//
// Every vacated row/column gets its OWN entropy — no shared strips.
//
// Operations act on the BMP view of the PCM buffer (the same layout
// pcmToBMP writes: 24-bit BGR, bottom-up rows, no padding). PCM bytes
// beyond the image grid ("tail") are carried through unchanged, and the
// result is trimmed back to the original PCM length, so length is
// invariant across applies.

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"math"
	"os"
	"sync"
)

// --------------------------------------------------------------- entropy --

var (
	entropyMu  sync.Mutex
	entropySrc io.Reader
	entropySet bool
)

// randomBytes fills p with fresh entropy from /dev/urandom. Falls back to
// crypto/rand only if the device is unavailable (e.g. non-Linux systems).
func randomBytes(p []byte) {
	entropyMu.Lock()
	defer entropyMu.Unlock()
	if !entropySet {
		if f, err := os.Open("/dev/urandom"); err == nil {
			entropySrc = bufio.NewReaderSize(f, 1<<16)
		} else {
			log.Printf("rotary: /dev/urandom unavailable, falling back to crypto/rand")
			entropySrc = bufio.NewReaderSize(rand.Reader, 1<<16)
		}
		entropySet = true
	}
	if _, err := io.ReadFull(entropySrc, p); err != nil {
		// Short/failed read from the buffered device — last-resort fallback.
		rand.Read(p)
	}
}

// ----------------------------------------------------------- image view ----

// pcmImage is the BMP-view of the PCM buffer. px holds h rows of rowSize
// bytes in STORAGE order (index 0 = bottom row, as written in a BMP file).
type pcmImage struct {
	w, h    int
	rowSize int    // w * 3
	px      []byte // h * rowSize
	tail    []byte // PCM bytes past the image grid, untouched
	origLen int    // original PCM length
}

func imageFromPCM(s *PCMState) *pcmImage {
	w, h := s.BMPDimensions()
	rowSize := w * 3
	usable := rowSize * h

	pad := (3 - len(s.Data)%3) % 3
	padded := make([]byte, len(s.Data)+pad)
	copy(padded, s.Data)

	img := &pcmImage{w: w, h: h, rowSize: rowSize, origLen: len(s.Data)}
	if usable <= len(padded) {
		img.px = padded[:usable]
		img.tail = padded[usable:]
	} else {
		// Tiny buffers where the grid outgrows the data — extend with zeros.
		px := make([]byte, usable)
		copy(px, padded)
		img.px = px
	}
	return img
}

// rowOff returns the byte offset of image row r (r = 0 is the TOP row).
func (im *pcmImage) rowOff(r int) int {
	return (im.h - 1 - r) * im.rowSize
}

// toPCM reassembles storage-order rows + tail and trims to original length.
func (im *pcmImage) toPCM() []byte {
	out := make([]byte, 0, im.origLen)
	out = append(out, im.px...)
	out = append(out, im.tail...)
	if len(out) > im.origLen {
		out = out[:im.origLen]
	}
	return out
}

// ---------------------------------------------------------------- shifts ---

func (im *pcmImage) shiftX(s int) {
	if s > im.w {
		s = im.w
	}
	if s < -im.w {
		s = -im.w
	}
	if s == 0 {
		return
	}
	n := s
	if n < 0 {
		n = -n
	}
	fresh := make([]byte, n*3)

	for r := 0; r < im.h; r++ {
		randomBytes(fresh) // fresh entropy PER ROW
		off := im.rowOff(r)
		row := im.px[off : off+im.rowSize]

		if s > 0 {
			// content moves right; vacate on the left
			for x := im.w - 1; x >= n; x-- {
				copy(row[x*3:x*3+3], row[(x-n)*3:(x-n)*3+3])
			}
			for x := 0; x < n; x++ {
				copy(row[x*3:x*3+3], fresh[x*3:x*3+3])
			}
		} else {
			// content moves left; vacate on the right
			for x := 0; x+n < im.w; x++ {
				copy(row[x*3:x*3+3], row[(x+n)*3:(x+n)*3+3])
			}
			for x := 0; x < n; x++ {
				dst := (im.w - n + x) * 3
				copy(row[dst:dst+3], fresh[x*3:x*3+3])
			}
		}
	}
}

func (im *pcmImage) shiftY(s int) {
	if s > im.h {
		s = im.h
	}
	if s < -im.h {
		s = -im.h
	}
	if s == 0 {
		return
	}
	n := s
	if n < 0 {
		n = -n
	}
	freshRow := make([]byte, im.rowSize)

	if s > 0 {
		// content moves DOWN; fresh rows at the TOP.
		// Iterate bottom-up so source rows (above) are read before written.
		for r := im.h - 1; r >= 0; r-- {
			off := im.rowOff(r)
			src := r - n
			if src < 0 {
				randomBytes(freshRow) // fresh entropy PER VACATED ROW
				copy(im.px[off:off+im.rowSize], freshRow)
			} else {
				soff := im.rowOff(src)
				copy(im.px[off:off+im.rowSize], im.px[soff:soff+im.rowSize])
			}
		}
	} else {
		// content moves UP; fresh rows at the BOTTOM.
		// Iterate top-down so source rows (below) are read before written.
		for r := 0; r < im.h; r++ {
			off := im.rowOff(r)
			src := r + n
			if src >= im.h {
				randomBytes(freshRow)
				copy(im.px[off:off+im.rowSize], freshRow)
			} else {
				soff := im.rowOff(src)
				copy(im.px[off:off+im.rowSize], im.px[soff:soff+im.rowSize])
			}
		}
	}
}

// ------------------------------------------------------------- randomize ---

func (im *pcmImage) randomize(pct float64) {
	if pct <= 0 {
		return
	}
	if pct > 100 {
		pct = 100
	}
	total := im.w * im.h
	count := int(math.Round(float64(total) * pct / 100.0))
	if count > total {
		count = total
	}
	if count <= 0 {
		return
	}

	// Uniform pixel selection: partial Fisher-Yates over all pixel indices.
	// Index draws use 4 bytes each; modulo bias is astronomically
	// irrelevant for artistic purposes.
	idx := make([]int, total)
	for i := range idx {
		idx[i] = i
	}
	draws := make([]byte, 4*count)
	randomBytes(draws)

	freshAll := make([]byte, 3*count)
	randomBytes(freshAll)

	for i := 0; i < count; i++ {
		j := i + int(binary.LittleEndian.Uint32(draws[4*i:4*i+4])%uint32(total-i))
		idx[i], idx[j] = idx[j], idx[i]
		p := idx[i]
		x, y := p%im.w, p/im.w
		off := im.rowOff(y) + x*3
		copy(im.px[off:off+3], freshAll[3*i:3*i+3])
	}
}

// ---------------------------------------------------------------- module ---

func applyTerrain(s *PCMState, params map[string]float64) error {
	if len(s.Data) == 0 {
		return errors.New("no data loaded")
	}
	sx := int(math.Round(params["shift_x_px"]))
	sy := int(math.Round(params["shift_y_px"]))
	pct := params["randomize_pct"]
	if sx == 0 && sy == 0 && pct <= 0 {
		return nil
	}

	img := imageFromPCM(s)
	if sx != 0 {
		img.shiftX(sx)
	}
	if sy != 0 {
		img.shiftY(sy)
	}
	if pct > 0 {
		img.randomize(pct)
	}
	s.Data = img.toPCM()
	return nil
}

func init() {
	RegisterModule(&Module{
		Name:     "terrain_apply",
		Category: "tool",
		Label:    "Terrain Shift",
		Params: []ParamSpec{
			{Name: "shift_x_px", Label: "Horizontal shift (px)", Min: -300, Max: 300, Step: 1, Def: 0, Kind: "number"},
			{Name: "shift_y_px", Label: "Vertical shift (px)", Min: -300, Max: 300, Step: 1, Def: 0, Kind: "number"},
			{Name: "randomize_pct", Label: "Randomize (%)", Min: 0, Max: 100, Step: 1, Def: 0, Kind: "number"},
		},
		Apply: applyTerrain,
	})
}
