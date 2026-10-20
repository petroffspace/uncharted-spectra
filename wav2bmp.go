//go:build ignore

package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: go run wav2bmp.go <input.wav> <output.bmp>")
		os.Exit(1)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Printf("Error reading WAV: %v\n", err)
		os.Exit(1)
	}

	if len(data) < 44 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		fmt.Println("Not a valid WAV file")
		os.Exit(1)
	}

	// Parse WAV format info
	sampleRate := binary.LittleEndian.Uint32(data[24:28])
	numChannels := binary.LittleEndian.Uint16(data[22:24])
	bitsPerSample := binary.LittleEndian.Uint16(data[34:36])

	// Find data chunk
	pcmStart, pcmLen := findDataChunk(data)
	if pcmStart == 0 {
		fmt.Println("No data chunk found")
		os.Exit(1)
	}

	pcm := data[pcmStart : pcmStart+pcmLen]
	originalLen := uint32(len(pcm))

	fmt.Printf("WAV: %d Hz, %d-bit, %d ch, %d bytes\n",
		sampleRate, bitsPerSample, numChannels, originalLen)

	// Pad to multiple of 3 (24-bit BMP = 3 bytes per pixel)
	padLen := (3 - len(pcm)%3) % 3
	padded := make([]byte, len(pcm)+padLen)
	copy(padded, pcm)

	totalPixels := len(padded) / 3

	// Calculate dimensions — roughly square
	width := int(math.Sqrt(float64(totalPixels)))
	// Ensure width*3 is divisible by 4 (eliminates row padding)
	for width > 0 && (width*3)%4 != 0 {
		width--
	}
	if width < 1 {
		width = 1
	}
	height := totalPixels / width
	if height < 1 {
		height = 1
	}

	usableBytes := width * height * 3

	fmt.Printf("BMP: %dx%d, 24-bit\n", width, height)

	// --- BMP File Header (14 bytes) ---
	fileHeader := make([]byte, 14)
	fileHeader[0] = 'B'
	fileHeader[1] = 'M'
	binary.LittleEndian.PutUint32(fileHeader[2:6], uint32(54+width*3*height))
	binary.LittleEndian.PutUint32(fileHeader[6:10], originalLen) // hide PCM length in reserved field
	binary.LittleEndian.PutUint32(fileHeader[10:14], 54)

	// --- BMP Info Header (40 bytes) ---
	info := make([]byte, 40)
	binary.LittleEndian.PutUint32(info[0:4], 40)
	binary.LittleEndian.PutUint32(info[4:8], uint32(width))
	binary.LittleEndian.PutUint32(info[8:12], uint32(height))
	binary.LittleEndian.PutUint16(info[12:14], 1)
	binary.LittleEndian.PutUint16(info[14:16], 24) // 24-bit
	binary.LittleEndian.PutUint32(info[16:20], 0)  // BI_RGB
	binary.LittleEndian.PutUint32(info[20:24], uint32(width*3*height))
	binary.LittleEndian.PutUint32(info[24:28], 2835)
	binary.LittleEndian.PutUint32(info[28:32], 2835)
	binary.LittleEndian.PutUint32(info[32:36], sampleRate)                                // hide sample rate in "colors used"
	binary.LittleEndian.PutUint32(info[36:40], uint32(numChannels)<<16|uint32(bitsPerSample)) // hide channels+bits in "important colors"

	// Write file
	f, err := os.Create(os.Args[2])
	if err != nil {
		fmt.Printf("Error creating BMP: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	f.Write(fileHeader)
	f.Write(info)

	// Write pixel data — bottom-up, raw bytes used directly as BGR pixels
	// No conversion, no transformation — bytes go in and come out identical
	rowSize := width * 3
	srcIdx := 0
	for y := height - 1; y >= 0; y-- {
		end := srcIdx + rowSize
		if end > len(padded) {
			end = len(padded)
		}
		row := padded[srcIdx:end]
		if len(row) < rowSize {
			fullRow := make([]byte, rowSize)
			copy(fullRow, row)
			row = fullRow
		}
		f.Write(row)
		srcIdx += rowSize
	}

	fmt.Printf("Written: %s (%d bytes of PCM data embedded)\n", os.Args[2], usableBytes)
}

func findDataChunk(data []byte) (int, int) {
	offset := 12
	for offset+8 <= len(data) {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		if id == "data" {
			return offset + 8, size
		}
		offset += 8 + size
		if size%2 != 0 {
			offset++
		}
	}
	return 0, 0
}
