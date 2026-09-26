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
		case "VP8 ":
			// A key frame's 3-byte tag, the start code, then 14-bit width
			// and height, each with 2 bits of scaling above.
			if size < 10 || payload[0]&0x01 != 0 || payload[3] != 0x9d || payload[4] != 0x01 || payload[5] != 0x2a {
				return webpFrame{}, ErrInvalid
			}
			frame.width = int(binary.LittleEndian.Uint16(payload[6:]) & 0x3fff)
			frame.height = int(binary.LittleEndian.Uint16(payload[8:]) & 0x3fff)
			return frame.checked()
		case "VP8L":
			// The signature, then 14 bits of width-1 and 14 of height-1.
			if size < 5 || payload[0] != 0x2f {
				return webpFrame{}, ErrInvalid
			}
			bits := binary.LittleEndian.Uint32(payload[1:])
			frame.width = int(bits&0x3fff) + 1
			frame.height = int(bits>>14&0x3fff) + 1
			frame.lossless = true
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
