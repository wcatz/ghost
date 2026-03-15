package voice

import (
	"encoding/binary"
	"math"
	"testing"
)

func generateSineFrame(freq, amplitude float64, sampleRate, durationMs int) []byte {
	samples := sampleRate * durationMs / 1000
	buf := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		t := float64(i) / float64(sampleRate)
		val := amplitude * math.Sin(2*math.Pi*freq*t)
		sample := int16(val)
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(sample))
	}
	return buf
}

func generateSilence(sampleRate, durationMs int) []byte {
	samples := sampleRate * durationMs / 1000
	return make([]byte, samples*2) // all zeros
}

func TestEnergyVAD_Silence(t *testing.T) {
	vad := NewEnergyVAD(800)
	frame := generateSilence(16000, 20)
	if vad.IsSpeech(frame) {
		t.Error("silence should not be detected as speech")
	}
}

func TestEnergyVAD_LoudSignal(t *testing.T) {
	vad := NewEnergyVAD(800)
	// 440 Hz sine at high amplitude.
	frame := generateSineFrame(440, 10000, 16000, 20)
	if !vad.IsSpeech(frame) {
		t.Error("loud signal should be detected as speech")
	}
}

func TestEnergyVAD_QuietSignal(t *testing.T) {
	vad := NewEnergyVAD(800)
	// Very quiet signal — below threshold.
	frame := generateSineFrame(440, 100, 16000, 20)
	if vad.IsSpeech(frame) {
		t.Error("quiet signal should not be detected as speech")
	}
}

func TestEnergyVAD_EmptyFrame(t *testing.T) {
	vad := NewEnergyVAD(800)
	if vad.IsSpeech(nil) {
		t.Error("nil frame should not be speech")
	}
	if vad.IsSpeech([]byte{0}) {
		t.Error("single byte frame should not be speech")
	}
}

func TestEnergyVAD_Reset(t *testing.T) {
	vad := NewEnergyVAD(800)
	vad.Reset() // should not panic
}

func TestEnergyVAD_DefaultThreshold(t *testing.T) {
	vad := NewEnergyVAD(0)
	// Should use default threshold of 800.
	silence := generateSilence(16000, 20)
	if vad.IsSpeech(silence) {
		t.Error("default threshold should reject silence")
	}
}
