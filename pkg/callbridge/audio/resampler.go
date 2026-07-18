package audio

// LinearResampler converts mono float32 PCM between fixed sample rates while
// preserving fractional position across input chunks.
type LinearResampler struct {
	inRate   int
	outRate  int
	position float64
	last     float32
	haveLast bool
}

func NewLinearResampler(inRate, outRate int) *LinearResampler {
	return &LinearResampler{inRate: inRate, outRate: outRate}
}

func (r *LinearResampler) Process(input []float32) []float32 {
	if len(input) == 0 || r.inRate <= 0 || r.outRate <= 0 {
		return nil
	}
	if r.inRate == r.outRate {
		output := make([]float32, len(input))
		copy(output, input)
		return output
	}

	var source []float32
	base := 0.0
	if r.haveLast {
		source = make([]float32, 0, len(input)+1)
		source = append(source, r.last)
		source = append(source, input...)
		base = 1
	} else {
		source = input
	}

	step := float64(r.inRate) / float64(r.outRate)
	capacity := int(float64(len(input))*float64(r.outRate)/float64(r.inRate)) + 1
	output := make([]float32, 0, capacity)
	for {
		index := r.position + base
		whole := int(index)
		if whole < 0 || whole+1 >= len(source) {
			break
		}
		fraction := float32(index - float64(whole))
		output = append(output, source[whole]*(1-fraction)+source[whole+1]*fraction)
		r.position += step
	}
	r.position -= float64(len(input))
	r.last = input[len(input)-1]
	r.haveLast = true
	return output
}
