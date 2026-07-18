package audio

import (
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/purpshell/meowcaller"
	opus "gopkg.in/hraban/opus.v2"
)

const (
	matrixSampleRate  = 48000
	matrixFrameSize   = 960
	maxOpusPacketSize = 4000
)

type OpusTrackSink struct {
	mu        sync.Mutex
	encoder   *opus.Encoder
	resampler *LinearResampler
	track     *webrtc.TrackLocalStaticSample
	pending   []float32
	closed    bool
}

func NewOpusTrackSink(track *webrtc.TrackLocalStaticSample, bitrate int) (*OpusTrackSink, error) {
	encoder, err := opus.NewEncoder(matrixSampleRate, 1, opus.AppVoIP)
	if err != nil {
		return nil, err
	}
	if err = encoder.SetBitrate(bitrate); err != nil {
		return nil, err
	}
	if err = encoder.SetInBandFEC(true); err != nil {
		return nil, err
	}
	if err = encoder.SetPacketLossPerc(5); err != nil {
		return nil, err
	}
	return &OpusTrackSink{
		encoder:   encoder,
		resampler: NewLinearResampler(meowcaller.SampleRate, matrixSampleRate),
		track:     track,
	}, nil
}

func (s *OpusTrackSink) WriteFrame(frame []float32) error {
	if len(frame) != meowcaller.FrameSamples {
		return errors.New("unexpected WhatsApp PCM frame size")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.pending = append(s.pending, s.resampler.Process(frame)...)
	packet := make([]byte, maxOpusPacketSize)
	for len(s.pending) >= matrixFrameSize {
		written, err := s.encoder.EncodeFloat32(s.pending[:matrixFrameSize], packet)
		if err != nil {
			return err
		}
		encoded := append([]byte(nil), packet[:written]...)
		if err = s.track.WriteSample(media.Sample{Data: encoded, Duration: 20 * time.Millisecond}); err != nil {
			return err
		}
		s.pending = s.pending[matrixFrameSize:]
	}
	return nil
}

func (s *OpusTrackSink) Close() error {
	s.mu.Lock()
	s.closed = true
	s.pending = nil
	s.mu.Unlock()
	return nil
}

type OpusTrackSource struct {
	mu        sync.Mutex
	decoder   *opus.Decoder
	resampler *LinearResampler
	queue     *FrameQueue
	pending   []float32
	closed    bool
}

func NewOpusTrackSource(queue *FrameQueue) (*OpusTrackSource, error) {
	decoder, err := opus.NewDecoder(matrixSampleRate, 1)
	if err != nil {
		return nil, err
	}
	return &OpusTrackSource{
		decoder:   decoder,
		resampler: NewLinearResampler(matrixSampleRate, meowcaller.SampleRate),
		queue:     queue,
	}, nil
}

func (s *OpusTrackSource) WritePacket(packet []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	decoded := make([]float32, 5760)
	samples, err := s.decoder.DecodeFloat32(packet, decoded)
	if err != nil {
		return err
	}
	s.pending = append(s.pending, s.resampler.Process(decoded[:samples])...)
	for len(s.pending) >= meowcaller.FrameSamples {
		s.queue.Push(s.pending[:meowcaller.FrameSamples])
		s.pending = s.pending[meowcaller.FrameSamples:]
	}
	return nil
}

func (s *OpusTrackSource) Close() error {
	s.mu.Lock()
	s.closed = true
	s.pending = nil
	s.mu.Unlock()
	return s.queue.Close()
}
