package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"time"
)

// Encoder is a running MP3 encoder: write raw mono 16-bit PCM to In, read MP3 from Out.
type Encoder struct {
	In   io.WriteCloser
	Out  io.ReadCloser
	wait func() error
}

// Wait waits for the encoder to exit and returns why it failed, if it did.
func (e *Encoder) Wait() error { return e.wait() }

// EncoderFactory starts an encoder that stops when ctx is cancelled.
type EncoderFactory func(ctx context.Context) (*Encoder, error)

// StartFFmpegEncoder starts ffmpeg encoding PCM from stdin to a constant-bitrate MP3 on stdout.
//
// -id3v2_version 0 drops the ID3 tag so the stream is nothing but frames (there is no -write_id3v2 option;
// ffmpeg accepts it and ignores it). The input flags matter: without nobuffer/probesize/analyzeduration ffmpeg held about a second of audio
// before encoding, which showed up as a one second delay on every level change.
func StartFFmpegEncoder(ctx context.Context) (*Encoder, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error",
		"-fflags", "nobuffer", "-probesize", "32", "-analyzeduration", "0",
		"-f", "s16le", "-ar", strconv.Itoa(SampleRate), "-ac", "1", "-i", "-",
		"-c:a", "libmp3lame", "-b:a", "128k",
		"-write_xing", "0", "-id3v2_version", "0", "-flush_packets", "1",
		"-f", "mp3", "-")
	cmd.WaitDelay = 3 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Encoder{In: in, Out: out, wait: func() error {
		err := cmd.Wait()
		if err != nil && ctx.Err() == nil {
			if msg := firstLine(stderr.String()); msg != "" {
				return errors.New("ffmpeg: " + msg)
			}
		}
		return err
	}}, nil
}

// NewTestEncoder builds an Encoder from its parts. It exists so tests in other packages can supply a fake;
// production code uses StartFFmpegEncoder.
func NewTestEncoder(in io.WriteCloser, out io.ReadCloser, wait func() error) *Encoder {
	return &Encoder{In: in, Out: out, wait: wait}
}
