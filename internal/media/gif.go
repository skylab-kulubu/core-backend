package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
)

// maxAnimationFrames is the most frames core keeps in an animated image.
const maxAnimationFrames = 300

// gifLayout is a GIF's logical screen and the rectangles of its frames,
// read from its blocks without decoding any image data.
type gifLayout struct {
	screen ImageSize
	frames []image.Rectangle
}

// decodeCost is what decoding every frame and making the still first frame
// takes: a byte a pixel for each frame, bounded by frames × screen, and a
// 4-byte-a-pixel canvas of the screen.
func (l gifLayout) decodeCost() int64 {
	screen := int64(l.screen.Width) * int64(l.screen.Height)
	return int64(len(l.frames))*screen + screen*4
}

// readGIFLayout walks a GIF's blocks. More than maxAnimationFrames frames,
// a frame outside the logical screen, or a block that does not read is
// ErrInvalid.
func readGIFLayout(data []byte) (gifLayout, error) {
	if len(data) < 13 || !isGIF(data) {
		return gifLayout{}, ErrInvalid
	}
	le := binary.LittleEndian
	layout := gifLayout{screen: ImageSize{Width: int(le.Uint16(data[6:])), Height: int(le.Uint16(data[8:]))}}
	if layout.screen.Width == 0 || layout.screen.Height == 0 {
		return gifLayout{}, ErrInvalid
	}
	screen := image.Rect(0, 0, layout.screen.Width, layout.screen.Height)
	pos := 13
	if data[10]&0x80 != 0 {
		pos += 3 * (1 << (int(data[10]&0x07) + 1))
	}
	for pos < len(data) {
		switch data[pos] {
		case 0x3B:
			if len(layout.frames) == 0 {
				return gifLayout{}, ErrInvalid
			}
			return layout, nil
		case 0x21:
			if pos+2 > len(data) {
				return gifLayout{}, ErrInvalid
			}
			pos = skipGIFSubBlocks(data, pos+2)
		case 0x2C:
			if pos+10 > len(data) {
				return gifLayout{}, ErrInvalid
			}
			x, y := int(le.Uint16(data[pos+1:])), int(le.Uint16(data[pos+3:]))
			frame := image.Rect(x, y, x+int(le.Uint16(data[pos+5:])), y+int(le.Uint16(data[pos+7:])))
			if frame.Empty() || !frame.In(screen) {
				return gifLayout{}, ErrInvalid
			}
			layout.frames = append(layout.frames, frame)
			if len(layout.frames) > maxAnimationFrames {
				return gifLayout{}, ErrInvalid
			}
			packed := data[pos+9]
			pos += 10
			if packed&0x80 != 0 {
				pos += 3 * (1 << (int(packed&0x07) + 1))
			}
			if pos+1 > len(data) {
				return gifLayout{}, ErrInvalid
			}
			pos = skipGIFSubBlocks(data, pos+1) // after the LZW code size
		default:
			return gifLayout{}, ErrInvalid
		}
		if pos < 0 {
			return gifLayout{}, ErrInvalid
		}
	}
	return gifLayout{}, ErrInvalid
}

// reencodeGIF re-encodes a GIF frame by frame, keeping its frames, their
// delays and disposal, and its loop count; comments and other extensions
// go. Its sizes are its first frame, still, as PNG. A GIF larger than the
// purpose's maximum dimension is scaled as a still image when it has one
// frame and refused when it is animated. The caller holds a decoding slot.
func reencodeGIF(data []byte, handling ImageHandling) (reencodedImage, error) {
	if err := checkDecode(data); err != nil {
		return reencodedImage{}, err
	}
	layout, _ := readGIFLayout(data)
	limit := handling.maxDimension()
	if layout.screen.Width > limit || layout.screen.Height > limit {
		if len(layout.frames) == 1 {
			return reencodeRaster(data, handling)
		}
		return reencodedImage{}, errImageTooLarge{maxPixels: int64(limit) * int64(limit)}
	}
	animation, err := decodeGIF(data)
	if err != nil {
		return reencodedImage{}, err
	}
	var body bytes.Buffer
	if err := gif.EncodeAll(&body, animation); err != nil {
		return reencodedImage{}, fmt.Errorf("media: re-encode GIF: %w", err)
	}
	first := image.NewRGBA(image.Rect(0, 0, layout.screen.Width, layout.screen.Height))
	draw.Draw(first, animation.Image[0].Bounds(), animation.Image[0], animation.Image[0].Bounds().Min, draw.Over)
	sizes, err := makeSizes(first, layout.screen, "image/png", handling.Sizes, nil)
	if err != nil {
		return reencodedImage{}, err
	}
	return reencodedImage{body: body.Bytes(), ctype: "image/gif", size: layout.screen, sizes: sizes, coverColors: coverColorsOf(first)}, nil
}

// decodeGIF decodes every frame; a decoder that panics refuses the file.
func decodeGIF(data []byte) (animation *gif.GIF, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			animation, err = nil, fmt.Errorf("%w: decoder panicked: %v", ErrInvalid, recovered)
		}
	}()
	animation, err = gif.DecodeAll(bytes.NewReader(data))
	if err != nil || len(animation.Image) == 0 {
		return nil, ErrInvalid
	}
	return animation, nil
}
