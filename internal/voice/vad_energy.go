package voice

import (
	"encoding/binary"
	"math"
)

// EnergyVAD is a simple energy-based voice activity detector.
// It computes the RMS energy of each audio frame and compares
// against a threshold. No external dependencies required.
type EnergyVAD struct {
	threshold float64
}

// NewEnergyVAD creates an energy-based VAD.
// threshold is the RMS energy level above which a frame is considered speech.
// A good default for 16-bit PCM is 500-1500 depending on mic gain.
func NewEnergyVAD(threshold float64) *EnergyVAD {
	if threshold <= 0 {
		threshold = 800
	}
	return &EnergyVAD{threshold: threshold}
}

// IsSpeech returns true if the RMS energy of the frame exceeds the threshold.
// frame must be 16-bit little-endian PCM audio.
func (v *EnergyVAD) IsSpeech(frame []byte) bool {
	if len(frame) < 2 {
		return false
	}

	samples := len(frame) / 2
	var sumSq float64
	for i := 0; i < samples; i++ {
		sample := int16(binary.LittleEndian.Uint16(frame[i*2 : i*2+2]))
		sumSq += float64(sample) * float64(sample)
	}
	rms := math.Sqrt(sumSq / float64(samples))
	return rms > v.threshold
}

// Reset clears internal state. EnergyVAD is stateless, so this is a no-op.
func (v *EnergyVAD) Reset() {}

// Close releases resources. EnergyVAD has nothing to release.
func (v *EnergyVAD) Close() error { return nil }
