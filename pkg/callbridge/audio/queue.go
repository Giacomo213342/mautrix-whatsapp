package audio

import (
	"io"
	"sync"

	"github.com/purpshell/meowcaller"
)

// FrameQueue is a bounded meowcaller audio source. When the producer falls
// behind, the oldest frame is discarded to keep call latency bounded.
type FrameQueue struct {
	frames    chan []float32
	closed    chan struct{}
	closeOnce sync.Once
}

func NewFrameQueue(capacity int) *FrameQueue {
	if capacity < 1 {
		capacity = 1
	}
	return &FrameQueue{
		frames: make(chan []float32, capacity),
		closed: make(chan struct{}),
	}
}

func (q *FrameQueue) Push(frame []float32) bool {
	if len(frame) != meowcaller.FrameSamples {
		return false
	}
	copyOfFrame := append([]float32(nil), frame...)
	select {
	case <-q.closed:
		return false
	default:
	}
	select {
	case q.frames <- copyOfFrame:
		return true
	default:
	}
	select {
	case <-q.frames:
	default:
	}
	select {
	case q.frames <- copyOfFrame:
		return true
	case <-q.closed:
		return false
	}
}

func (q *FrameQueue) ReadFrame() ([]float32, error) {
	select {
	case frame := <-q.frames:
		return frame, nil
	case <-q.closed:
		return nil, io.EOF
	default:
		return make([]float32, meowcaller.FrameSamples), nil
	}
}

func (q *FrameQueue) Close() error {
	q.closeOnce.Do(func() {
		close(q.closed)
	})
	return nil
}
