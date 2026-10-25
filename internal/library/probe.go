package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// Info is what probing a file tells us.
type Info struct {
	Duration   float64 // seconds
	Codec      string
	SampleRate int
	Channels   int
}

// ProbeFunc inspects one audio file. It must fail for anything that is not a usable audio file.
type ProbeFunc func(ctx context.Context, path string) (Info, error)

const probeTimeout = 15 * time.Second

// FFProbe probes a file with the ffprobe binary on PATH.
func FFProbe(ctx context.Context, path string) (Info, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	// "--" is not needed: ffprobe takes the file as the last argument, and path is absolute.
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name,sample_rate,channels:format=duration",
		"-of", "json", path).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return Info{}, fmt.Errorf("ffprobe: %s", firstLine(string(ee.Stderr)))
		}
		if ctx.Err() != nil {
			return Info{}, fmt.Errorf("ffprobe: %w", ctx.Err())
		}
		return Info{}, fmt.Errorf("ffprobe: %w", err)
	}
	return parseProbe(out)
}

type probeJSON struct {
	Streams []struct {
		Codec      string `json:"codec_name"`
		SampleRate string `json:"sample_rate"`
		Channels   int    `json:"channels"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func parseProbe(b []byte) (Info, error) {
	var p probeJSON
	if err := json.Unmarshal(b, &p); err != nil {
		return Info{}, fmt.Errorf("ffprobe output: %w", err)
	}
	if len(p.Streams) == 0 {
		return Info{}, errors.New("no audio stream")
	}
	s := p.Streams[0]
	dur, _ := strconv.ParseFloat(p.Format.Duration, 64)
	if dur <= 0 {
		return Info{}, errors.New("unknown or zero duration")
	}
	rate, _ := strconv.Atoi(s.SampleRate)
	return Info{Duration: dur, Codec: s.Codec, SampleRate: rate, Channels: s.Channels}, nil
}

// ffContext matches the "[mp3 @ 0x7197a57ce500] " prefix ffmpeg puts on messages: noise for a user, and
// the address changes on every run.
var ffContext = regexp.MustCompile(`^\[[^\]]*@ 0x[0-9a-fA-F]+\] `)

// firstLine returns the first line of s with any ffmpeg context prefix removed.
func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' || r == '\r' {
			s = s[:i]
			break
		}
	}
	return ffContext.ReplaceAllString(s, "")
}
