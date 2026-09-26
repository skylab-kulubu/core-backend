package media

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
)

// jpegQuality is the quality core encodes JPEG images at.
const jpegQuality = 85

// reencodedImage is a raster image after core decoded it and encoded its
// pixels again: nothing the uploader put around or inside the pixels (EXIF,
// comments, trailing bytes, polyglot payloads) survives.
type reencodedImage struct {
	body  []byte
	ctype string
	size  ImageSize
	// sizes are the image's sizes it is larger than, encoded as ctype.
	sizes []encodedSize
	// coverColors are the image's cover colours (coverColorsOf).
	coverColors []string
}

// encodedSize is one of an image's sizes (SizeCard, SizePage), encoded to
// be stored as its own object.
type encodedSize struct {
	name  string
	body  []byte
	ctype string
	size  ImageSize
}

// sizeObjects is how the sizes are recorded on the Media.
func sizeObjectsOf(sizes []encodedSize) map[string]SizeObject {
	out := make(map[string]SizeObject, len(sizes))
	for _, s := range sizes {
		out[s.name] = SizeObject{ImageSize: s.size, Type: s.ctype}
	}
	return out
}

// reencodeRaster decodes a raster image of one of the rasterFormats, scales
// it down to fit the purpose's maximum dimension (MaxImageDimension when
// the purpose sets none), turns it upright by its EXIF Orientation, encodes
// it again, and makes each of the purpose's sizes it is larger than. The
// caller holds a decoding slot.
func reencodeRaster(data []byte, handling ImageHandling) (reencodedImage, error) {
	img, err := decodeRaster(data)
	if err != nil {
		return reencodedImage{}, err
	}
	// Scaled first, then turned: the limit is a square, so the turned image
	// fits it too, and a 48 MP photo is never turned at full size.
	img = orient(fitWithin(img, handling.maxDimension()), jpegOrientation(data))
	return finishImage(img, outputType(detectContentType(data), img), handling.Sizes)
}

// finishImage encodes an upright image that core stores in place of what
// was uploaded, and makes its sizes and cover colours.
func finishImage(img image.Image, ctype string, sizes map[string]int) (reencodedImage, error) {
	body, err := encodeRaster(img, ctype)
	if err != nil {
		return reencodedImage{}, err
	}
	encoded, err := makeSizes(img, sizeOf(img), ctype, sizes, nil)
	if err != nil {
		return reencodedImage{}, err
	}
	return reencodedImage{body: body, ctype: ctype, size: sizeOf(img), sizes: encoded, coverColors: coverColorsOf(img)}, nil
}

// makeSizes makes each of the sizes (size name to its longer side in
// pixels) that an image of size shown is larger than, from img: the image
// upright, maybe already scaled down to the largest size.
func makeSizes(img image.Image, shown ImageSize, ctype string, sizes map[string]int, icc []byte) ([]encodedSize, error) {
	var out []encodedSize
	for _, name := range imageSizes {
		px, ok := sizes[name]
		if !ok || (shown.Width <= px && shown.Height <= px) {
			continue
		}
		scaled := fitWithin(img, px)
		body, err := encodeRaster(scaled, ctype)
		if err != nil {
			return nil, err
		}
		out = append(out, encodedSize{name: name, body: body, ctype: ctype, size: sizeOf(scaled)})
	}
	return out, nil
}

// sizesOfStored makes the sizes of an image already stored, which is not
// encoded again: the size backfill's step. It reports the image's size
// as shown (upright). The image is scaled to the largest size before it is
// turned. The caller holds a decoding slot.
func sizesOfStored(data []byte, sizes map[string]int) (ImageSize, []encodedSize, error) {
	img, err := decodeRaster(data)
	if err != nil {
		return ImageSize{}, nil, err
	}
	orientation := jpegOrientation(data)
	shown := sizeOf(img)
	if orientation >= 5 {
		shown = ImageSize{Width: shown.Height, Height: shown.Width}
	}
	largest := 0
	for _, px := range sizes {
		largest = max(largest, px)
	}
	work := orient(fitWithin(img, largest), orientation)
	encoded, err := makeSizes(work, shown, outputType(detectContentType(data), work), sizes, nil)
	if err != nil {
		return ImageSize{}, nil, err
	}
	return shown, encoded, nil
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
