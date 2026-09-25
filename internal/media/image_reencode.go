package media

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"

	"golang.org/x/image/draw"
)

// jpegQuality is the quality core encodes JPEG images at.
const jpegQuality = 85

// MaxImagePixels is the most pixels core decodes in one image: a 48 MP
// phone photo fits. The header's claim is checked before anything is
// decoded, so an image that claims more (a decompression bomb) costs
// nothing to refuse.
const MaxImagePixels = 50_000_000

// maxDecodedImageBytes is the most memory one decoded image may take. An
// image with deep pixels (16 bits a channel) fits fewer of them than
// MaxImagePixels.
const maxDecodedImageBytes = 256 << 20

// errTooManyPixels refuses an image larger than core decodes. limit is the
// most pixels an image of its kind may have.
type errTooManyPixels struct{ limit int64 }

func (e errTooManyPixels) Error() string { return "media: image has too many pixels" }

// checkPixels reads only the image's header and refuses it when decoding it
// would take more than core allows.
func checkPixels(data []byte) error {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return ErrInvalid
	}
	limit := min(int64(MaxImagePixels), int64(maxDecodedImageBytes/bytesPerPixel(config.ColorModel)))
	if int64(config.Width)*int64(config.Height) > limit {
		return errTooManyPixels{limit: limit}
	}
	return nil
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

// reencodedImage is a raster image after core decoded it and encoded its
// pixels again: nothing the uploader put around or inside the pixels (EXIF,
// comments, trailing bytes, polyglot payloads) survives.
type reencodedImage struct {
	body          []byte
	ctype         string
	width, height int
	// variants are the sizes smaller than the image, encoded the same way.
	variants []encodedVariant
}

// encodedVariant is one stored size of an image.
type encodedVariant struct {
	size          string
	body          []byte
	width, height int
}

// storedSizes is how the variants are recorded on the Media.
func (r reencodedImage) storedSizes() map[string]ImageSize {
	out := make(map[string]ImageSize, len(r.variants))
	for _, v := range r.variants {
		out[v.size] = ImageSize{Width: v.width, Height: v.height}
	}
	return out
}

// reencodeRaster decodes a raster image of one of the rasterFormats, scales
// it down to fit the purpose's maximum dimension (MaxImageDimension when
// the purpose sets none), encodes it again, and makes each of the purpose's
// sizes the image is larger than.
func reencodeRaster(data []byte, handling ImageHandling) (reencodedImage, error) {
	if err := checkPixels(data); err != nil {
		return reencodedImage{}, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return reencodedImage{}, ErrInvalid
	}
	limit := handling.MaxDimension
	if limit <= 0 {
		limit = MaxImageDimension
	}
	// Scaled first, then turned: the limit is a square, so the turned image
	// fits it too, and turning the smaller image is cheaper.
	img = orient(fitWithin(img, limit), jpegOrientation(data))
	ctype := outputType(detectContentType(data))
	body, err := encodeRaster(img, ctype)
	if err != nil {
		return reencodedImage{}, err
	}
	bounds := img.Bounds()
	out := reencodedImage{body: body, ctype: ctype, width: bounds.Dx(), height: bounds.Dy()}
	for _, size := range imageSizes {
		px, ok := handling.Variants[size]
		if !ok || (out.width <= px && out.height <= px) {
			continue
		}
		scaled := fitWithin(img, px)
		variantBody, err := encodeRaster(scaled, ctype)
		if err != nil {
			return reencodedImage{}, err
		}
		out.variants = append(out.variants, encodedVariant{size: size, body: variantBody, width: scaled.Bounds().Dx(), height: scaled.Bounds().Dy()})
	}
	return out, nil
}

// fittedSize is w×h scaled down, keeping its proportions, so that neither
// side is above limit. A size that fits is kept.
func fittedSize(w, h, limit int) (int, int) {
	if w <= limit && h <= limit {
		return w, h
	}
	scale := float64(limit) / float64(max(w, h))
	return max(1, int(math.Round(float64(w)*scale))), max(1, int(math.Round(float64(h)*scale)))
}

// fitWithin scales img down to fittedSize. An image that fits is returned
// as it is.
func fitWithin(img image.Image, limit int) image.Image {
	bounds := img.Bounds()
	w, h := fittedSize(bounds.Dx(), bounds.Dy(), limit)
	if w == bounds.Dx() && h == bounds.Dy() {
		return img
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Src, nil)
	return dst
}

// outputType is the type core stores an image of the given type as.
func outputType(uploaded string) string {
	if uploaded == "image/jpeg" {
		return "image/jpeg"
	}
	return "image/png"
}

func encodeRaster(img image.Image, ctype string) ([]byte, error) {
	var out bytes.Buffer
	var err error
	if ctype == "image/jpeg" {
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: jpegQuality})
	} else {
		err = png.Encode(&out, img)
	}
	return out.Bytes(), err
}
