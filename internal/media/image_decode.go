package media

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
)

// MaxImagePixels is the most pixels core decodes in one image: a 48 MP
// phone photo fits.
const MaxImagePixels = 50_000_000

// maxDecodedImageBytes is the most memory decoding one image may take, by
// the estimate decodeCost makes from the header. An image with costly
// pixels (16 bits a channel, a progressive JPEG's coefficients) fits fewer
// of them than MaxImagePixels.
const maxDecodedImageBytes = 256 << 20

// errImageTooLarge refuses an image whose decoding would take more than
// core allows. maxPixels is the most pixels an image like it may have.
type errImageTooLarge struct{ maxPixels int64 }

func (e errImageTooLarge) Error() string { return "media: image too large to decode" }

// checkDecode reads only the image's header, and the markers a decoder
// would follow, and refuses it before anything is decoded when decoding it
// would take more than core allows. Content that does not read as one of
// the raster formats is ErrInvalid.
func checkDecode(data []byte) error {
	pixels, cost, err := decodeCost(data)
	if err != nil {
		return err
	}
	return refuseCost(pixels, cost)
}

// refuseCost refuses an image of pixels pixels whose decoding takes cost
// bytes when either is above what core allows.
func refuseCost(pixels, cost int64) error {
	if pixels > MaxImagePixels || cost > maxDecodedImageBytes {
		perPixel := max(1, cost/max(1, pixels))
		return errImageTooLarge{maxPixels: min(int64(MaxImagePixels), maxDecodedImageBytes/perPixel)}
	}
	return nil
}

// decodeCost is how many pixels the image has and how many bytes the
// decoder allocates for it, estimated per format from what the decoder
// would read before allocating:
//
//   - JPEG: the sample planes of its frame (SOF), and for a progressive
//     JPEG the coefficients it keeps between scans (256 bytes a block);
//     a CMYK JPEG is also converted, 4 bytes a pixel. More than
//     maxJPEGScans scans is ErrInvalid.
//   - PNG: the image's pixels at their depth, twice for an interlaced one.
//   - GIF: every frame, a byte a pixel, counted as frames × logical
//     screen, and a canvas of the screen for the still first frame; more
//     than maxAnimationFrames frames, or a frame outside the screen, is
//     ErrInvalid (readGIFLayout).
//   - WebP: the frame its bitstream declares, which must be the canvas an
//     extended WebP declares (webpFrame), and its alpha.
func decodeCost(data []byte) (pixels, cost int64, err error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return 0, 0, ErrInvalid
	}
	pixels = int64(config.Width) * int64(config.Height)
	switch format {
	case "jpeg":
		frame, ok := readJPEGFrame(data)
		if !ok || jpegScans(data) > maxJPEGScans {
			return 0, 0, ErrInvalid
		}
		return pixels, frame.decodeCost(), nil
	case "png":
		cost = pixels * int64(bytesPerPixel(config.ColorModel))
		if pngInterlaced(data) {
			cost *= 2
		}
		return pixels, cost, nil
	case "gif":
		layout, err := readGIFLayout(data)
		if err != nil {
			return 0, 0, err
		}
		if err := checkAnimation(len(layout.frames), layout.screen); err != nil {
			return 0, 0, err
		}
		return pixels, layout.decodeCost(), nil
	case "webp":
		frame, err := readWebPFrame(data)
		if err != nil {
			return 0, 0, err
		}
		return frame.pixels(), frame.decodeCost(), nil
	}
	return 0, 0, ErrInvalid
}

// bytesPerPixel is how much memory a decoder takes for a pixel of the
// model, at most.
func bytesPerPixel(model color.Model) int {
	switch model {
	case color.RGBA64Model, color.NRGBA64Model:
		return 8
	case color.Gray16Model:
		return 2
	case color.GrayModel, color.AlphaModel:
		return 1
	case color.YCbCrModel:
		return 3
	}
	if _, paletted := model.(color.Palette); paletted {
		return 1
	}
	return 4
}

// pngInterlaced reports whether a PNG is Adam7-interlaced: its decoder then
// keeps each pass apart before assembling the image.
func pngInterlaced(data []byte) bool {
	// Signature (8), IHDR length and type (8), width, height (8), depth,
	// colour type, compression, filter (4), then the interlace method.
	return len(data) > 28 && data[28] == 1
}

// decodeRaster decodes a raster image after checkDecode allows it. A
// decoder that panics on a hostile file refuses the file (ErrInvalid)
// instead of stopping core.
func decodeRaster(data []byte) (img image.Image, err error) {
	if err := checkDecode(data); err != nil {
		return nil, err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			img, err = nil, fmt.Errorf("%w: decoder panicked: %v", ErrInvalid, recovered)
		}
	}()
	img, _, err = image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrInvalid
	}
	return img, nil
}
