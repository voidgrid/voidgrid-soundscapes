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

// Decoder reads an audio file and returns it as mono 16-bit samples at SampleRate.
type Decoder func(ctx context.Context, path string) ([]int16, error)

// FFmpegDecode decodes any file ffmpeg can read, downmixing to mono and resampling to SampleRate.
func FFmpegDecode(ctx context.Context, path string) ([]int16, error) {
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

	limit := int64(MaxDecodeSeconds) * SampleRate * 2
	raw, rerr := io.ReadAll(io.LimitReader(out, limit+1))
	tooLong := int64(len(raw)) > limit
	if tooLong {
		cancel() // stop ffmpeg decoding the rest
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

	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(uint16(raw[2*i]) | uint16(raw[2*i+1])<<8)
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
