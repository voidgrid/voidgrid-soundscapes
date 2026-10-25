package engine

// FrameSplitter cuts a byte stream of MPEG Layer III audio into whole frames. Listeners are given whole
// frames only, so one that connects in the middle of the stream starts on a frame boundary and its
// decoder does not have to hunt for one.
//
// A candidate header is only accepted when the frame it describes is followed by another valid header, so
// stray bytes that happen to look like a header are skipped.
type FrameSplitter struct {
	buf []byte
}

var (
	bitratesV1 = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	bitratesV2 = [16]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
	ratesV1    = [4]int{44100, 48000, 32000, 0}
	ratesV2    = [4]int{22050, 24000, 16000, 0}
	ratesV25   = [4]int{11025, 12000, 8000, 0}
)

// frameLen returns the length in bytes of the Layer III frame whose header starts at b, or 0 if b does not
// start a valid header.
func frameLen(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return 0
	}
	version := (b[1] >> 3) & 3 // 3 = MPEG1, 2 = MPEG2, 0 = MPEG2.5, 1 = reserved
	layer := (b[1] >> 1) & 3   // 1 = Layer III
	if version == 1 || layer != 1 {
		return 0
	}
	bri, sri := int(b[2]>>4), int((b[2]>>2)&3)
	pad := int((b[2] >> 1) & 1)
	var bitrate, rate, coef int
	switch version {
	case 3:
		bitrate, rate, coef = bitratesV1[bri], ratesV1[sri], 144
	case 2:
		bitrate, rate, coef = bitratesV2[bri], ratesV2[sri], 72
	default:
		bitrate, rate, coef = bitratesV2[bri], ratesV25[sri], 72
	}
	if bitrate == 0 || rate == 0 {
		return 0
	}
	return coef*bitrate*1000/rate + pad
}

// Write adds data and returns every complete frame now available. Each returned slice is newly allocated
// and safe to keep and share.
func (s *FrameSplitter) Write(p []byte) [][]byte {
	s.buf = append(s.buf, p...)
	var frames [][]byte
	i := 0
	for {
		// find the next sync byte
		for i < len(s.buf) && s.buf[i] != 0xFF {
			i++
		}
		if len(s.buf)-i < 4 {
			break
		}
		n := frameLen(s.buf[i:])
		if n == 0 {
			i++
			continue
		}
		if len(s.buf)-i < n+4 {
			break // wait for the whole frame and the next header, to be sure this one is real
		}
		if frameLen(s.buf[i+n:]) == 0 {
			i++ // not followed by a header: a false sync
			continue
		}
		frames = append(frames, append([]byte(nil), s.buf[i:i+n]...))
		i += n
	}
	// keep only the unconsumed tail
	s.buf = append(s.buf[:0], s.buf[i:]...)
	return frames
}
