package audio

import (
	"math"
	"testing"
)

func TestLinearResamplerKeepsLongTermRate(t *testing.T) {
	up := NewLinearResampler(16000, 48000)
	var output int
	for range 100 {
		output += len(up.Process(make([]float32, 960)))
	}
	if difference := int(math.Abs(float64(output - 288000))); difference > 3 {
		t.Fatalf("resampled sample count drifted by %d: got %d", difference, output)
	}
}

func TestLinearResamplerDownsamplesMatrixFrame(t *testing.T) {
	down := NewLinearResampler(48000, 16000)
	input := make([]float32, 960)
	for index := range input {
		input[index] = float32(index)
	}
	output := down.Process(input)
	if len(output) != 320 {
		t.Fatalf("expected 320 output samples, got %d", len(output))
	}
	for index, sample := range output {
		if sample != float32(index*3) {
			t.Fatalf("sample %d: expected %d, got %f", index, index*3, sample)
		}
	}
}
