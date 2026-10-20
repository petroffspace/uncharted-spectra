//go:build ignore

package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: go run bmp2wav.go <input.bmp> <output.wav>")
		os.Exit(1)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Printf("Error reading BMP: %v\n", err)
		os.Exit(1)
	}

	if len(data) < 54 || data[0] != 'B' || data[1] != 'M' {
		fmt.Println("Not a valid BMP file")
		os.Exit(1)
	}

	// Parse BMP header
	width := int(binary.LittleEndian.Uint32(data[18:22]))
	height := int(binary.LittleEndian.Uint32(data[22:26]))
	bitsPerPixel := int(binary.LittleEndian.Uint16(data[28:30]))
	pixelOffset := int(binary.LittleEndian.Uint32(data[10:14]))

	// Recover hidden format info from BMP header fields
	originalPCMLen := binary.LittleEndian.Uint32(data[6:10])   // from reserved field
	sampleRate := binary.LittleEndian.Uint32(data[46:50])      // from "colors used"
	formatField := binary.LittleEndian.Uint32(data[50:54])     // from "important colors"
	numChannels := uint16(formatField >> 16)
	bitsPerSample := uint16(formatField & 0xFFFF)

	// Fallbacks if fields are zero (BMP not created by our tool)
	if sampleRate == 0 {
		sampleRate = 44100
	}
	if numChannels == 0 {
		numChannels = 1
	}
	if bitsPerSample == 0 {
		bitsPerSample = 16
	}

	rowSize := width * bitsPerPixel / 8
	rowPadding := (4 - rowSize%4) % 4
	paddedRowSize := rowSize + rowPadding

	fmt.Printf("BMP: %dx%d, %d-bit\n", width, height, bitsPerPixel)
	fmt.Printf("Recovered format: %d Hz, %d-bit, %d ch, %d bytes\n",
		sampleRate, bitsPerSample, numChannels, originalPCMLen)

	// Extract pixel data — reverse bottom-up order, skip row padding
	pixelData := data[pixelOffset:]
	rawBytes := make([]byte, 0, rowSize*height)

	for y := 0; y < height; y++ {
		fileRow := (height - 1 - y) * paddedRowSize
		end := fileRow + rowSize
		if end > len(pixelData) {
			end = len(pixelData)
		}
		if fileRow >= len(pixelData) {
			break
		}
		rawBytes = append(rawBytes, pixelData[fileRow:end]...)
	}

	// Trim to original PCM length (removes any padding we added)
	if originalPCMLen > 0 && int(originalPCMLen) <= len(rawBytes) {
		rawBytes = rawBytes[:originalPCMLen]
	}

	fmt.Printf("Extracted %d bytes\n", len(rawBytes))

	writeWAV(rawBytes, sampleRate, numChannels, bitsPerSample, os.Args[2])
	fmt.Printf("Written: %s\n", os.Args[2])
}

func writeWAV(pcm []byte, sampleRate uint32, numChannels, bitsPerSample uint16, path string) {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	byteRate := sampleRate * uint32(numChannels) * uint32(bitsPerSample) / 8
	blockAlign := numChannels * bitsPerSample / 8
	dataSize := uint32(len(pcm))
	chunkSize := 36 + dataSize

	// RIFF header
	f.Write([]byte{'R', 'I', 'F', 'F'})
	tmp4 := make([]byte, 4)
	binary.LittleEndian.PutUint32(tmp4, chunkSize)
	f.Write(tmp4)
	f.Write([]byte{'W', 'A', 'V', 'E'})

	// fmt chunk
	f.Write([]byte{'f', 'm', 't', ' '})
	binary.LittleEndian.PutUint32(tmp4, 16)
	f.Write(tmp4)
	tmp2 := make([]byte, 2)
	binary.LittleEndian.PutUint16(tmp2, 1) // PCM
	f.Write(tmp2)
	binary.LittleEndian.PutUint16(tmp2, numChannels)
	f.Write(tmp2)
	binary.LittleEndian.PutUint32(tmp4, sampleRate)
	f.Write(tmp4)
	binary.LittleEndian.PutUint32(tmp4, byteRate)
	f.Write(tmp4)
	binary.LittleEndian.PutUint16(tmp2, blockAlign)
	f.Write(tmp2)
	binary.LittleEndian.PutUint16(tmp2, bitsPerSample)
	f.Write(tmp2)

	// data chunk
	f.Write([]byte{'d', 'a', 't', 'a'})
	binary.LittleEndian.PutUint32(tmp4, dataSize)
	f.Write(tmp4)
	f.Write(pcm)
}
