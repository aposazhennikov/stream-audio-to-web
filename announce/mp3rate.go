package announce

import (
	"io"
	"os"
	"sync"
)

// DefaultSampleRate is the rate clips and sounds are made at when a track's rate is unknown.
const DefaultSampleRate = 44100

// The receiver plays a stream at the sample rate of its first frames and goes silent (and
// reconnects) when the rate changes. Clips put before the station audio must therefore have
// the sample rate of the track that follows them.

var (
	mp3Rates = map[byte][3]int{ // MPEG version bits -> sample rates
		3: {44100, 48000, 32000}, // MPEG-1
		2: {22050, 24000, 16000}, // MPEG-2
		0: {11025, 12000, 8000},  // MPEG-2.5
	}
	mp3Bitrates1 = [15]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	mp3Bitrates2 = [15]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
)

type rateKey struct {
	path  string
	size  int64
	mtime int64
}

var rateCache sync.Map // rateKey -> int

// SampleRate returns the sample rate of an MP3 file's first audio frame, 0 if it cannot tell.
func SampleRate(path string) int {
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	key := rateKey{path, info.Size(), info.ModTime().UnixNano()}
	if rate, ok := rateCache.Load(key); ok {
		return rate.(int)
	}
	rate := readSampleRate(path)
	rateCache.Store(key, rate)
	return rate
}

func readSampleRate(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	head := make([]byte, 10)
	if _, err := io.ReadFull(f, head); err != nil {
		return 0
	}
	offset := int64(0)
	if string(head[:3]) == "ID3" {
		offset = 10 + int64(head[6]&0x7f)<<21 | int64(head[7]&0x7f)<<14 | int64(head[8]&0x7f)<<7 | int64(head[9]&0x7f)
		if head[5]&0x10 != 0 { // footer present
			offset += 10
		}
	}
	buf := make([]byte, 64*1024)
	n, _ := f.ReadAt(buf, offset)
	return FrameSampleRate(buf[:n])
}

// FrameSampleRate finds the first MPEG audio Layer III frame in data, confirmed by the frame
// header that follows it, and returns its sample rate (0 if none).
func FrameSampleRate(data []byte) int {
	for i := 0; i+4 <= len(data); i++ {
		rate, size, ok := frameHeader(data[i:])
		if !ok {
			continue
		}
		next := i + size
		if next+4 > len(data) {
			// The frame runs to the end of what was read: accept it if it is the only one.
			if i == 0 || next == len(data) {
				return rate
			}
			continue
		}
		if nextRate, _, ok := frameHeader(data[next:]); ok && nextRate == rate {
			return rate
		}
	}
	return 0
}

// frameHeader parses a Layer III frame header: its sample rate and frame length in bytes.
func frameHeader(b []byte) (int, int, bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return 0, 0, false
	}
	version := (b[1] >> 3) & 3
	layer := (b[1] >> 1) & 3
	bitrateIndex := b[2] >> 4
	rateIndex := (b[2] >> 2) & 3
	padding := int((b[2] >> 1) & 1)
	rates, ok := mp3Rates[version]
	if !ok || layer != 1 || bitrateIndex == 0 || bitrateIndex == 15 || rateIndex == 3 {
		return 0, 0, false
	}
	rate := rates[rateIndex]
	if version == 3 {
		return rate, 144*mp3Bitrates1[bitrateIndex]*1000/rate + padding, true
	}
	return rate, 72*mp3Bitrates2[bitrateIndex]*1000/rate + padding, true
}

// ClipRate is the rate to make clips at for a track of this rate: the default when unknown.
func ClipRate(trackRate int) int {
	if trackRate <= 0 {
		return DefaultSampleRate
	}
	return trackRate
}
