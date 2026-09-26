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

// checkPixels reads only the image's header (and, for a JPEG, its segment
// markers) and refuses it when decoding it would take more than core
// allows.
func checkPixels(data []byte) error {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return ErrInvalid
	}
	if isJPEG(data) && jpegScans(data) > maxJPEGScans {
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
	variants []encodedSize
}

// encodedSize is one stored size of an image.
type encodedSize struct {
	size          string
	body          []byte
	ctype         string
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
	img, err := decodeRaster(data)
	if err != nil {
		return reencodedImage{}, err
	}
	limit := handling.MaxDimension
	if limit <= 0 {
		limit = MaxImageDimension
	}
	// Scaled first, then turned: the limit is a square, so the turned image
	// fits it too, and turning the smaller image is cheaper.
	img = orient(fitWithin(img, limit), jpegOrientation(data))
	ctype := outputType(detectContentType(data), img)
	body, err := encodeRaster(img, ctype)
	if err != nil {
		return reencodedImage{}, err
	}
	variants, err := makeSizes(img, ctype, handling.Sizes)
	if err != nil {
		return reencodedImage{}, err
	}
	bounds := img.Bounds()
	return reencodedImage{body: body, ctype: ctype, width: bounds.Dx(), height: bounds.Dy(), variants: variants}, nil
}

// decodeRaster decodes a raster image after checking from its header that
// core may decode it (checkPixels).
func decodeRaster(data []byte) (image.Image, error) {
	if err := checkPixels(data); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrInvalid
	}
	return img, nil
}

// makeSizes makes each of the sizes (size name to its longer side in
// pixels) that the upright image is larger than, encoded as ctype.
func makeSizes(img image.Image, ctype string, sizes map[string]int) ([]encodedSize, error) {
	var out []encodedSize
	bounds := img.Bounds()
	for _, size := range imageSizes {
		px, ok := sizes[size]
		if !ok || (bounds.Dx() <= px && bounds.Dy() <= px) {
			continue
		}
		scaled := fitWithin(img, px)
		body, err := encodeRaster(scaled, ctype)
		if err != nil {
			return nil, err
		}
		out = append(out, encodedSize{size: size, body: body, ctype: ctype, width: scaled.Bounds().Dx(), height: scaled.Bounds().Dy()})
	}
	return out, nil
}

// keptImage is what core makes of an image whose own bytes it keeps (a
// Media uploaded without a purpose): its size as shown, and its sizes.
type keptImage struct {
	size     ImageSize
	variants []encodedSize
}

// keptImageSizes makes the sizes of an image whose own bytes core keeps:
// the original stays as it is, and only the sizes are made from it,
// upright. An image core cannot decode gets no size and no sizes.
func keptImageSizes(data []byte, sizes map[string]int) keptImage {
	img, err := decodeRaster(data)
	if err != nil {
		return keptImage{}
	}
	img = orient(img, jpegOrientation(data))
	ctype := outputType(detectContentType(data), img)
	variants, err := makeSizes(img, ctype, sizes)
	if err != nil {
		return keptImage{}
	}
	return keptImage{size: ImageSize{Width: img.Bounds().Dx(), Height: img.Bounds().Dy()}, variants: variants}
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

// outputType is the type core stores an image uploaded as the given type
// as. Go has no WebP encoder, so a WebP becomes a JPEG when it is opaque
// (a photo) and a PNG when it has transparency. A GIF becomes a PNG of its
// first frame. Everything else keeps its type.
func outputType(uploaded string, img image.Image) string {
	switch uploaded {
	case "image/jpeg":
		return "image/jpeg"
	case "image/webp":
		if opaque, ok := img.(interface{ Opaque() bool }); ok && opaque.Opaque() {
			return "image/jpeg"
		}
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
