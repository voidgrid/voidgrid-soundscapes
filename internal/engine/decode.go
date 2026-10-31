package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"time"
)

const (
	// MaxDecodeSeconds caps how much of one file is decoded into memory (mono, 16-bit: about 5 MB per minute).
	MaxDecodeSeconds = 20 * 60
	decodeTimeout    = 3 * time.Minute
)

// Decoder reads an audio file and returns it as mono 16-bit samples at SampleRate. seconds is the
// duration the library probed (0 if unknown); it only sizes the buffer, so a wrong value is harmless.
type Decoder func(ctx context.Context, path string, seconds float64) ([]int16, error)

// readBlock is how many bytes of ffmpeg output are read at a time.
const readBlock = 64 << 10

// FFmpegDecode decodes any file ffmpeg can read, downmixing to mono and resampling to SampleRate. The
// samples are read straight into one slice sized from seconds, so a file is held in memory once rather
// than as a byte buffer plus a sample copy (and a regrown buffer for each doubling).
func FFmpegDecode(ctx context.Context, path string, seconds float64) ([]int16, error) {
	ctx, cancel := context.WithTimeout(ctx, decodeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-nostdin", "-i", path,
		"-vn", "-f", "s16le", "-ac", "1", "-ar", strconv.Itoa(SampleRate), "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	limit := MaxDecodeSeconds * SampleRate // samples
	// A second of slack covers resampling and a duration that was only an estimate.
	pcm := make([]int16, 0, min(max(int(seconds*SampleRate), 0)+SampleRate, limit+1))
	buf := make([]byte, readBlock)
	held := 0 // bytes at the start of buf that are half a sample
	var rerr error
	tooLong := false
	for {
		n, err := out.Read(buf[held:])
		n += held
		pairs := n / 2
		for i := 0; i < pairs; i++ {
			pcm = append(pcm, int16(uint16(buf[2*i])|uint16(buf[2*i+1])<<8))
		}
		held = n - 2*pairs
		if held == 1 {
			buf[0] = buf[n-1]
		}
		if len(pcm) > limit {
			tooLong = true
			cancel() // stop ffmpeg decoding the rest
			break
		}
		if err != nil {
			if err != io.EOF {
				rerr = err
			}
			break
		}
	}
	werr := cmd.Wait()

	switch {
	case tooLong:
		return nil, fmt.Errorf("longer than %d minutes", MaxDecodeSeconds/60)
	case rerr != nil:
		return nil, rerr
	case werr != nil:
		if ctx.Err() != nil {
			return nil, fmt.Errorf("decoding timed out: %w", ctx.Err())
		}
		msg := firstLine(stderr.String())
		if msg == "" {
			msg = werr.Error()
		}
		return nil, errors.New("ffmpeg: " + msg)
	}
	return pcm, nil
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' || r == '\r' {
			return s[:i]
		}
	}
	return s
}
