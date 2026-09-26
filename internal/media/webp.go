package media

import "encoding/binary"

// webpFrame is what a WebP's bitstream declares, read without decoding it.
type webpFrame struct {
	width, height int
	// lossless is a VP8L frame; otherwise VP8 (lossy).
	lossless bool
	// canvasWidth and canvasHeight are an extended WebP's VP8X canvas (0
	// without one): the alpha plane is sized by it.
	canvasWidth, canvasHeight int
	// alpha is the ALPH chunk's compression: -1 without one, 0 raw, 1
	// VP8L-compressed.
	alpha int
}

func (f webpFrame) pixels() int64 { return int64(f.width) * int64(f.height) }

// decodeCost is what golang.org/x/image/webp allocates: a VP8 frame's
// YCbCr planes and working rows (2 bytes a pixel is generous), a VP8L
// frame's NRGBA image and its transform buffers (8 bytes a pixel), and an
// alpha plane over the canvas (5 bytes a pixel when it is VP8L-compressed:
// an NRGBA image, then the plane).
func (f webpFrame) decodeCost() int64 {
	cost := f.pixels() * 2
	if f.lossless {
		cost = f.pixels() * 8
	}
	canvas := int64(f.canvasWidth) * int64(f.canvasHeight)
	switch f.alpha {
	case 0:
		cost += canvas
	case 1:
		cost += canvas * 5
	}
	return cost
}

// readWebPFrame reads a WebP's chunks up to its first frame. image.
// DecodeConfig reports an extended WebP's VP8X canvas, but the decoder
// allocates the frame its VP8 or VP8L bitstream declares, which a hostile
// file makes far larger (a 1×1 canvas over an 8000×8000 frame). A frame
// that is not the canvas is ErrInvalid, as is an animation, which the
// decoder does not read anyway.
func readWebPFrame(data []byte) (webpFrame, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return webpFrame{}, ErrInvalid
	}
	frame := webpFrame{alpha: -1}
	pos := 12
	for pos+8 <= len(data) {
		fourcc := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4:]))
		payload := data[pos+8:]
		if size < 0 || size > len(payload) {
			return webpFrame{}, ErrInvalid
		}
		payload = payload[:size]
		switch fourcc {
		case "VP8X":
			if size < 10 || payload[0]&0x02 != 0 { // the animation flag
				return webpFrame{}, ErrInvalid
			}
			frame.canvasWidth = int(uint32(payload[4])|uint32(payload[5])<<8|uint32(payload[6])<<16) + 1
			frame.canvasHeight = int(uint32(payload[7])|uint32(payload[8])<<8|uint32(payload[9])<<16) + 1
		case "ANIM", "ANMF":
			return webpFrame{}, ErrInvalid
		case "ALPH":
			if size < 1 {
				return webpFrame{}, ErrInvalid
			}
			frame.alpha = int(payload[0] & 0x03)
		case "VP8 ", "VP8L":
			var ok bool
			frame.width, frame.height, ok = webpBitstreamSize(fourcc, payload)
			if !ok {
				return webpFrame{}, ErrInvalid
			}
			frame.lossless = fourcc == "VP8L"
			return frame.checked()
		}
		pos += 8 + size + size&1
	}
	return webpFrame{}, ErrInvalid
}

func (f webpFrame) checked() (webpFrame, error) {
	if f.width <= 0 || f.height <= 0 {
		return webpFrame{}, ErrInvalid
	}
	if f.canvasWidth != 0 && (f.width != f.canvasWidth || f.height != f.canvasHeight) {
		return webpFrame{}, ErrInvalid
	}
	return f, nil
}

// webpBitstreamSize is the size a VP8 or VP8L bitstream declares.
func webpBitstreamSize(fourcc string, payload []byte) (width, height int, ok bool) {
	switch fourcc {
	case "VP8 ":
		// A key frame's 3-byte tag, the start code, then 14-bit width and
		// height, each with 2 bits of scaling above.
		if len(payload) < 10 || payload[0]&0x01 != 0 || payload[3] != 0x9d || payload[4] != 0x01 || payload[5] != 0x2a {
			return 0, 0, false
		}
		return int(binary.LittleEndian.Uint16(payload[6:]) & 0x3fff), int(binary.LittleEndian.Uint16(payload[8:]) & 0x3fff), true
	case "VP8L":
		// The signature, then 14 bits of width-1 and 14 of height-1.
		if len(payload) < 5 || payload[0] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(payload[1:])
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, true
	}
	return 0, 0, false
}

// maxAnimationPixels is the most pixels an animated WebP may show across
// its frames, counted as frames × canvas: a browser composes every frame
// over the whole canvas.
const maxAnimationPixels = 256 << 20

// isAnimatedWebP reports whether data is an extended WebP whose VP8X
// chunk, which must come first, sets the animation flag.
func isAnimatedWebP(data []byte) bool {
	return len(data) >= 21 && isWebP(data) && string(data[12:16]) == "VP8X" && data[20]&0x02 != 0
}

// VP8X flags for metadata a kept WebP loses.
const (
	webpEXIFFlag = 0x08
	webpXMPFlag  = 0x04
)

// cleanAnimatedWebP checks an animated WebP chunk by chunk and returns it
// without its EXIF and XMP chunks, with its RIFF size and VP8X flags
// rewritten. Go has no WebP encoder, so this is the one image core keeps
// as uploaded: a RIFF size that is not the file's (trailing or missing
// bytes), a chunk other than VP8X, ICCP, ANIM, ANMF, EXIF and XMP, a frame
// that leaves the canvas or whose bitstream is not the size its ANMF
// declares, or more than maxAnimationFrames frames is ErrInvalid; a canvas
// above the purpose's maximum dimension, or more than maxAnimationPixels
// frames × canvas pixels, is errImageTooLarge.
func cleanAnimatedWebP(data []byte, limit int) ([]byte, ImageSize, error) {
	le := binary.LittleEndian
	if !isAnimatedWebP(data) || int(le.Uint32(data[4:]))+8 != len(data) {
		return nil, ImageSize{}, ErrInvalid
	}
	var canvas ImageSize
	var kept [][]byte
	var vp8x []byte
	frames, seenANIM := 0, false
	for pos := 12; pos < len(data); {
		if pos+8 > len(data) {
			return nil, ImageSize{}, ErrInvalid
		}
		fourcc := string(data[pos : pos+4])
		size := int(le.Uint32(data[pos+4:]))
		end := pos + 8 + size + size&1
		if size < 0 || end > len(data) {
			return nil, ImageSize{}, ErrInvalid
		}
		chunk, payload := data[pos:end], data[pos+8:pos+8+size]
		switch {
		case fourcc == "VP8X" && pos == 12 && size == 10:
			canvas = ImageSize{
				Width:  int(uint32(payload[4])|uint32(payload[5])<<8|uint32(payload[6])<<16) + 1,
				Height: int(uint32(payload[7])|uint32(payload[8])<<8|uint32(payload[9])<<16) + 1,
			}
			if canvas.Width > limit || canvas.Height > limit {
				return nil, ImageSize{}, errImageTooLarge{maxPixels: int64(limit) * int64(limit)}
			}
			vp8x = append([]byte{}, chunk...)
			vp8x[8] &^= webpEXIFFlag | webpXMPFlag
			kept = append(kept, vp8x)
		case fourcc == "ICCP" && !seenANIM:
			kept = append(kept, chunk)
		case fourcc == "ANIM" && !seenANIM && size == 6:
			seenANIM = true
			kept = append(kept, chunk)
		case fourcc == "ANMF" && seenANIM:
			if err := checkAnimationFrame(payload, canvas); err != nil {
				return nil, ImageSize{}, err
			}
			frames++
			if frames > maxAnimationFrames {
				return nil, ImageSize{}, ErrInvalid
			}
			kept = append(kept, chunk)
		case fourcc == "EXIF", fourcc == "XMP ":
			// Metadata goes.
		default:
			return nil, ImageSize{}, ErrInvalid
		}
		pos = end
	}
	if frames == 0 {
		return nil, ImageSize{}, ErrInvalid
	}
	shown := int64(frames) * int64(canvas.Width) * int64(canvas.Height)
	if shown > maxAnimationPixels {
		return nil, ImageSize{}, errImageTooLarge{maxPixels: maxAnimationPixels / int64(frames)}
	}
	body := []byte("WEBP")
	for _, chunk := range kept {
		body = append(body, chunk...)
	}
	out := append([]byte("RIFF"), le.AppendUint32(nil, uint32(len(body)))...)
	return append(out, body...), canvas, nil
}

// checkAnimationFrame checks an ANMF payload: its rectangle within the
// canvas, and its frame data an optional ALPH chunk then one VP8 or VP8L
// bitstream of the size the ANMF declares.
func checkAnimationFrame(payload []byte, canvas ImageSize) error {
	if len(payload) < 16 {
		return ErrInvalid
	}
	u24 := func(b []byte) int { return int(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16) }
	x, y := 2*u24(payload[0:]), 2*u24(payload[3:])
	w, h := u24(payload[6:])+1, u24(payload[9:])+1
	if x+w > canvas.Width || y+h > canvas.Height {
		return ErrInvalid
	}
	le := binary.LittleEndian
	seenAlpha := false
	for pos := 16; pos < len(payload); {
		if pos+8 > len(payload) {
			return ErrInvalid
		}
		fourcc := string(payload[pos : pos+4])
		size := int(le.Uint32(payload[pos+4:]))
		end := pos + 8 + size + size&1
		if size < 0 || end > len(payload) {
			return ErrInvalid
		}
		switch fourcc {
		case "ALPH":
			if seenAlpha {
				return ErrInvalid
			}
			seenAlpha = true
		case "VP8 ", "VP8L":
			fw, fh, ok := webpBitstreamSize(fourcc, payload[pos+8:pos+8+size])
			if !ok || fw != w || fh != h || end != len(payload) {
				return ErrInvalid
			}
			return nil
		default:
			return ErrInvalid
		}
		pos = end
	}
	return ErrInvalid
}
