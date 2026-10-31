// Package engine mixes layers into MP3 streams.
//
// The pieces: BuildLoop turns decoded audio into a gapless loop, Mixer sums loops with live gain, Mix feeds
// one ffmpeg MP3 encoder per soundscape and fans the frames out to listeners, and Engine owns the mixes.
package engine

import (
	"errors"
	"math"
	"time"
)

const (
	// SampleRate is the rate everything is mixed at. Every layer is resampled to it by ffmpeg when decoded.
	SampleRate = 44100
	// ChunkSamples is how many samples are mixed and encoded per step (about 23 ms).
	ChunkSamples = 1024
)

// ErrSilent is returned for a file with no audible content.
var ErrSilent = errors.New("no audible content")

// LoopOptions control how a decoded file becomes a loop.
type LoopOptions struct {
	// Crossfade is the length of the equal-power crossfade that joins the end of the file to its start.
	// Zero joins them with no fade (a hard cut). It is shortened for files under three times its length.
	Crossfade time.Duration
	// SilenceThreshold is the largest sample magnitude still treated as silence when trimming the ends.
	SilenceThreshold int16
}

// DefaultLoopOptions are the settings used when none are given.
var DefaultLoopOptions = LoopOptions{Crossfade: 1500 * time.Millisecond, SilenceThreshold: 8}

// BuildLoop trims digital silence from both ends of pcm and crossfades the tail into the head, so the
// result can be played round and round with no gap and no click. The input is not modified.
//
// Why: an MP3 decodes with silent padding at both ends, and some recordings carry a silent tail of their
// own (one file measured 1.6 s). Looping the raw decode would drop out at every seam.
func BuildLoop(pcm []int16, opt LoopOptions) ([]int16, error) {
	return buildLoop(pcm, opt, false)
}

// BuildLoopConsuming is BuildLoop for a caller that is done with pcm: it builds the loop inside pcm's
// own memory instead of a second copy, so pcm must not be used afterwards. The result is a slice of pcm.
func BuildLoopConsuming(pcm []int16, opt LoopOptions) ([]int16, error) {
	return buildLoop(pcm, opt, true)
}

func buildLoop(pcm []int16, opt LoopOptions, inPlace bool) ([]int16, error) {
	a, b := 0, len(pcm)
	for a < b && abs16(pcm[a]) <= opt.SilenceThreshold {
		a++
	}
	for b > a && abs16(pcm[b-1]) <= opt.SilenceThreshold {
		b--
	}
	if a == b {
		return nil, ErrSilent
	}
	src := pcm[a:b]

	x := int(opt.Crossfade * SampleRate / time.Second)
	if x > len(src)/3 {
		x = len(src) / 3
	}
	if x < 2 {
		if inPlace {
			return src, nil
		}
		return append([]int16(nil), src...), nil
	}

	// The loop is the file up to a join point, with the head blended over its first x samples with the
	// stretch of audio just before the join point. The sample before the wrap is src[q-1] and the one after
	// it is src[q], the true continuation, so the join is continuous.
	//
	// The join point q normally sits x samples from the end. For tonal material (a hum, a fan, a drone) the
	// two stretches being blended can be out of phase and cancel, so q is moved back by up to
	// maxAlignShift to where the tail lines up best with the head. And the blend itself follows how alike
	// the two stretches are: a linear fade when they match (so they add up to the right level) and an
	// equal-power fade when they do not (noise), instead of always the latter.
	q := len(src) - x
	maxShift := min(maxAlignShift, len(src)/4)
	shift, _ := bestShift(src, q, x, maxShift)
	q -= shift
	rho := correlation(src[:x], src[q:q+x])

	// Only the first x samples change and they read src[i] and src[q+i]. x <= len/3 and shift <= len/4, so
	// q >= 5/12 of the length is past x: writing in place never overwrites a sample that is still to be read.
	var out []int16
	if inPlace {
		out = src[:q:q]
	} else {
		out = make([]int16, q)
		copy(out, src[:q])
	}
	for i := 0; i < x; i++ {
		t := float64(i) / float64(x)
		in := (1-rho)*math.Sin(t*math.Pi/2) + rho*t
		fo := (1-rho)*math.Cos(t*math.Pi/2) + rho*(1-t)
		out[i] = clamp16(float64(src[i])*in + float64(src[q+i])*fo)
	}
	return out, nil
}

const (
	// maxAlignShift is how far (half a second) the join may move to find a better match. A search over this
	// range costs well under a second of CPU for each file, once, when it is first loaded.
	maxAlignShift = SampleRate / 2
	// alignWindow is how many samples of the head are compared at each candidate position.
	alignWindow = 4096
	// alignMinCorrelation is how alike head and tail must be before the join is moved. Unrelated noise
	// scores about 0.02 at this window size, so this only moves the join for clearly periodic material.
	alignMinCorrelation = 0.5
)

// bestShift finds how far before q the tail should start so that it lines up best with the start of src.
// It returns 0 unless the match is clearly better than chance.
func bestShift(src []int16, q, x, maxShift int) (shift int, rho float64) {
	w := min(alignWindow, x)
	head := src[:w]
	var eh float64
	for _, v := range head {
		eh += float64(v) * float64(v)
	}
	if eh == 0 {
		return 0, 0
	}
	best, bestRho := 0, -2.0
	for s := 0; s <= maxShift; s++ {
		tail := src[q-s : q-s+w]
		var dot, et float64
		for i, v := range tail {
			dot += float64(v) * float64(head[i])
			et += float64(v) * float64(v)
		}
		if et == 0 {
			continue
		}
		if r := dot / math.Sqrt(eh*et); r > bestRho {
			best, bestRho = s, r
		}
	}
	if bestRho < alignMinCorrelation {
		return 0, bestRho
	}
	return best, bestRho
}

// correlation returns how alike a and b are, from 0 (unrelated or opposite) to 1 (identical in shape).
func correlation(a, b []int16) float64 {
	var dot, ea, eb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		ea += float64(a[i]) * float64(a[i])
		eb += float64(b[i]) * float64(b[i])
	}
	if ea == 0 || eb == 0 {
		return 0
	}
	return math.Max(0, math.Min(1, dot/math.Sqrt(ea*eb)))
}

func abs16(v int16) int16 {
	if v < 0 {
		if v == math.MinInt16 {
			return math.MaxInt16
		}
		return -v
	}
	return v
}

func clamp16(v float64) int16 {
	switch {
	case v > math.MaxInt16:
		return math.MaxInt16
	case v < math.MinInt16:
		return math.MinInt16
	}
	return int16(math.Round(v))
}
