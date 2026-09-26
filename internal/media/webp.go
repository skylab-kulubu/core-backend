package media

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"golang.org/x/image/webp"
)

// riffChunk is one chunk of a RIFF container: its type, its payload and
// the whole chunk as stored (header, payload, padding).
type riffChunk struct {
	fourcc  string
	payload []byte
	raw     []byte
}

// riffChunks reads the chunks of a RIFF body in order. A chunk that runs
// past the end of the body, or a body that ends inside a chunk header, is
// ErrInvalid. It is the one walk of WebP chunks: the frame header, the
// colour profile, animations and their frames all read chunks through it.
func riffChunks(body []byte) ([]riffChunk, error) {
	var chunks []riffChunk
	for pos := 0; pos < len(body); {
		if pos+8 > len(body) {
			return nil, ErrInvalid
		}
		size := int(binary.LittleEndian.Uint32(body[pos+4:]))
		end := pos + 8 + size + size&1
		if end > len(body) {
			return nil, ErrInvalid
		}
		chunks = append(chunks, riffChunk{fourcc: string(body[pos : pos+4]), payload: body[pos+8 : pos+8+size], raw: body[pos:end]})
		pos = end
	}
	return chunks, nil
}

// webpChunks are the chunks of a WebP file, within its RIFF size (bytes
// after it are left out). ErrInvalid when it is not a WebP.
func webpChunks(data []byte) ([]riffChunk, error) {
	if !isWebP(data) {
		return nil, ErrInvalid
	}
	end := min(len(data), 8+int(binary.LittleEndian.Uint32(data[4:])))
	if end < 12 {
		return nil, ErrInvalid
	}
	return riffChunks(data[12:end])
}

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
	chunks, err := webpChunks(data)
	if err != nil {
		return webpFrame{}, err
	}
	frame := webpFrame{alpha: -1}
	for _, chunk := range chunks {
		switch chunk.fourcc {
		case "VP8X":
			if len(chunk.payload) < 10 || chunk.payload[0]&webpAnimationFlag != 0 {
				return webpFrame{}, ErrInvalid
			}
			frame.canvasWidth, frame.canvasHeight = webpCanvas(chunk.payload)
		case "ANIM", "ANMF":
			return webpFrame{}, ErrInvalid
		case "ALPH":
			if len(chunk.payload) < 1 {
				return webpFrame{}, ErrInvalid
			}
			frame.alpha = int(chunk.payload[0] & 0x03)
		case "VP8 ", "VP8L":
			var ok bool
			frame.width, frame.height, ok = webpBitstreamSize(chunk.fourcc, chunk.payload)
			if !ok || frame.width <= 0 || frame.height <= 0 {
				return webpFrame{}, ErrInvalid
			}
			if frame.canvasWidth != 0 && (frame.width != frame.canvasWidth || frame.height != frame.canvasHeight) {
				return webpFrame{}, ErrInvalid
			}
			frame.lossless = chunk.fourcc == "VP8L"
			return frame, nil
		}
	}
	return webpFrame{}, ErrInvalid
}

// webpCanvas is the canvas a VP8X payload declares.
func webpCanvas(vp8x []byte) (int, int) {
	return int(uint24(vp8x[4:])) + 1, int(uint24(vp8x[7:])) + 1
}

func uint24(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
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

// VP8X flags.
const (
	webpAnimationFlag = 0x02
	webpXMPFlag       = 0x04
	webpEXIFFlag      = 0x08
	webpAlphaFlag     = 0x10
	webpICCFlag       = 0x20
)

// isAnimatedWebP reports whether data is an extended WebP whose VP8X
// chunk, which must come first, sets the animation flag.
func isAnimatedWebP(data []byte) bool {
	return len(data) >= 21 && isWebP(data) && string(data[12:16]) == "VP8X" && data[20]&webpAnimationFlag != 0
}

// animationFrame is an ANMF frame: its rectangle and its frame data (an
// optional ALPH chunk, then a VP8 or VP8L bitstream).
type animationFrame struct {
	x, y, width, height int
	alpha, bitstream    *riffChunk
}

// cleanAnimatedWebP checks an animated WebP and returns it without its
// EXIF and XMP chunks, an invalid colour profile dropped, its RIFF size
// and VP8X flags rewritten. Go has no WebP encoder, so this
// is the one image core keeps as uploaded, once its structure is checked
// and every frame decodes:
//
//   - the RIFF size must be the file's (no trailing or missing bytes);
//   - only VP8X (first, animation flag set), ICCP, ANIM, ANMF, EXIF and
//     XMP chunks;
//   - each frame within the canvas, its bitstream the size its ANMF
//     declares, and decoding to that size;
//   - at most maxAnimationFrames frames and maxAnimationPixels frames ×
//     canvas pixels (checkAnimation), a canvas within limit.
//
// A structure that does not check, or a frame that does not decode, is
// ErrInvalid; a canvas above limit or too many pixels is errImageTooLarge.
// The frames are decoded one after another; the caller holds a decoding
// slot.
func cleanAnimatedWebP(data []byte, limit int) ([]byte, ImageSize, error) {
	if !isAnimatedWebP(data) || int(binary.LittleEndian.Uint32(data[4:]))+8 != len(data) {
		return nil, ImageSize{}, ErrInvalid
	}
	chunks, err := riffChunks(data[12:])
	if err != nil || chunks[0].fourcc != "VP8X" || len(chunks[0].payload) != 10 {
		return nil, ImageSize{}, ErrInvalid
	}
	var canvas ImageSize
	canvas.Width, canvas.Height = webpCanvas(chunks[0].payload)
	if canvas.Width > limit || canvas.Height > limit {
		return nil, ImageSize{}, errImageTooLarge{maxPixels: int64(limit) * int64(limit)}
	}
	vp8x := append([]byte{}, chunks[0].raw...)
	vp8x[8] &^= webpEXIFFlag | webpXMPFlag | webpICCFlag
	kept := [][]byte{vp8x}
	var frames []animationFrame
	seenANIM := false
	for _, chunk := range chunks[1:] {
		switch {
		case chunk.fourcc == "ICCP" && !seenANIM:
			if validICC(chunk.payload) {
				vp8x[8] |= webpICCFlag
				kept = append(kept, chunk.raw)
			}
		case chunk.fourcc == "ANIM" && !seenANIM && len(chunk.payload) == 6:
			seenANIM = true
			kept = append(kept, chunk.raw)
		case chunk.fourcc == "ANMF" && seenANIM:
			frame, err := readAnimationFrame(chunk.payload, canvas)
			if err != nil {
				return nil, ImageSize{}, err
			}
			frames = append(frames, frame)
			if len(frames) > maxAnimationFrames {
				return nil, ImageSize{}, ErrInvalid
			}
			kept = append(kept, chunk.raw)
		case chunk.fourcc == "EXIF", chunk.fourcc == "XMP ":
			// Metadata goes.
		default:
			return nil, ImageSize{}, ErrInvalid
		}
	}
	if len(frames) == 0 {
		return nil, ImageSize{}, ErrInvalid
	}
	if err := checkAnimation(len(frames), canvas); err != nil {
		return nil, ImageSize{}, err
	}
	for _, frame := range frames {
		if err := frame.decodes(); err != nil {
			return nil, ImageSize{}, err
		}
	}
	body := []byte("WEBP")
	for _, chunk := range kept {
		body = append(body, chunk...)
	}
	out := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	return append(out, body...), canvas, nil
}

// readAnimationFrame reads an ANMF payload: its rectangle within the
// canvas, and its frame data an optional ALPH chunk then one VP8 or VP8L
// bitstream of the size the ANMF declares.
func readAnimationFrame(payload []byte, canvas ImageSize) (animationFrame, error) {
	if len(payload) < 16 {
		return animationFrame{}, ErrInvalid
	}
	frame := animationFrame{
		x: 2 * int(uint24(payload[0:])), y: 2 * int(uint24(payload[3:])),
		width: int(uint24(payload[6:])) + 1, height: int(uint24(payload[9:])) + 1,
	}
	if frame.x+frame.width > canvas.Width || frame.y+frame.height > canvas.Height {
		return animationFrame{}, ErrInvalid
	}
	chunks, err := riffChunks(payload[16:])
	if err != nil {
		return animationFrame{}, err
	}
	for i := range chunks {
		chunk := &chunks[i]
		switch {
		case chunk.fourcc == "ALPH" && frame.alpha == nil && frame.bitstream == nil:
			frame.alpha = chunk
		case (chunk.fourcc == "VP8 " || chunk.fourcc == "VP8L") && frame.bitstream == nil && i == len(chunks)-1:
			w, h, ok := webpBitstreamSize(chunk.fourcc, chunk.payload)
			if !ok || w != frame.width || h != frame.height || (chunk.fourcc == "VP8L" && frame.alpha != nil) {
				return animationFrame{}, ErrInvalid
			}
			frame.bitstream = chunk
		default:
			return animationFrame{}, ErrInvalid
		}
	}
	if frame.bitstream == nil {
		return animationFrame{}, ErrInvalid
	}
	return frame, nil
}

// decodes decodes the frame on its own, as a still WebP of its bitstream
// (and its alpha), and checks it is the size its ANMF declares. A decoder
// that panics refuses the frame.
func (f animationFrame) decodes() (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: decoder panicked: %v", ErrInvalid, recovered)
		}
	}()
	body := []byte("WEBP")
	if f.alpha != nil {
		vp8x := []byte{webpAlphaFlag, 0, 0, 0}
		vp8x = append(vp8x, byte(f.width-1), byte((f.width-1)>>8), byte((f.width-1)>>16))
		vp8x = append(vp8x, byte(f.height-1), byte((f.height-1)>>8), byte((f.height-1)>>16))
		body = append(body, webpChunkOf("VP8X", vp8x)...)
		body = append(body, f.alpha.raw...)
	}
	body = append(body, f.bitstream.raw...)
	still := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	img, err := webp.Decode(bytes.NewReader(append(still, body...)))
	if err != nil || img.Bounds().Dx() != f.width || img.Bounds().Dy() != f.height {
		return ErrInvalid
	}
	return nil
}

// webpChunkOf is a RIFF chunk of the payload, padded to an even length.
func webpChunkOf(fourcc string, payload []byte) []byte {
	out := append([]byte(fourcc), binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))...)
	out = append(out, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0)
	}
	return out
}
