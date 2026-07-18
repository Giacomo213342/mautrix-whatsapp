package audio

import (
	"io"
	"testing"

	"github.com/purpshell/meowcaller"
)

func TestFrameQueueDropsOldest(t *testing.T) {
	queue := NewFrameQueue(1)
	first := make([]float32, meowcaller.FrameSamples)
	second := make([]float32, meowcaller.FrameSamples)
	first[0] = 1
	second[0] = 2
	if !queue.Push(first) || !queue.Push(second) {
		t.Fatal("failed to push valid frames")
	}
	got, err := queue.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 2 {
		t.Fatalf("expected newest frame, got marker %f", got[0])
	}
}

func TestFrameQueueReturnsSilenceWhenEmptyAndEOFAfterClose(t *testing.T) {
	queue := NewFrameQueue(1)
	frame, err := queue.ReadFrame()
	if err != nil || len(frame) != meowcaller.FrameSamples {
		t.Fatalf("expected silence frame, got len=%d err=%v", len(frame), err)
	}
	_ = queue.Close()
	if _, err = queue.ReadFrame(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}
