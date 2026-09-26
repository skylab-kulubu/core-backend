package media

import (
	"image"
	"math"

	"golang.org/x/image/draw"
)

// maxScaleBufferBytes bounds the intermediate image downscale works on.
const maxScaleBufferBytes = 128 << 20

// fittedSize is size scaled down, keeping its proportions, so that neither
// side is above limit. A size that fits is kept.
func fittedSize(size ImageSize, limit int) ImageSize {
	if size.Width <= limit && size.Height <= limit {
		return size
	}
	scale := float64(limit) / float64(max(size.Width, size.Height))
	return ImageSize{
		Width:  max(1, int(math.Round(float64(size.Width)*scale))),
		Height: max(1, int(math.Round(float64(size.Height)*scale))),
	}
}

func sizeOf(img image.Image) ImageSize {
	return ImageSize{Width: img.Bounds().Dx(), Height: img.Bounds().Dy()}
}

// fitWithin scales img down to fittedSize. An image that fits is returned
// as it is.
func fitWithin(img image.Image, limit int) image.Image {
	target := fittedSize(sizeOf(img), limit)
	if target == sizeOf(img) {
		return img
	}
	return downscale(img, target)
}

// downscale scales img down to size with memory that does not grow with
// the source. A kernel scaler (Catmull-Rom) keeps a float64 buffer of
// source height × target width, half a gigabyte for a 48 MP photo; instead
// a bilinear pass, which reads the source in place, brings the image to 2^k
// times the target (k ≤ 2, within maxScaleBufferBytes and never above the
// source), and exact 2×2 averages halve it k times. Each bilinear step
// shrinks by less than 2 when the source allows, so no source pixel is
// skipped.
func downscale(img image.Image, size ImageSize) *image.RGBA {
	src := sizeOf(img)
	k := 0
	for k < 2 {
		next := ImageSize{Width: size.Width << (k + 1), Height: size.Height << (k + 1)}
		if next.Width > src.Width || next.Height > src.Height || int64(next.Width)*int64(next.Height)*4 > maxScaleBufferBytes {
			break
		}
		k++
	}
	scaled := image.NewRGBA(image.Rect(0, 0, size.Width<<k, size.Height<<k))
	draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), img, img.Bounds(), draw.Src, nil)
	for ; k > 0; k-- {
		scaled = halve(scaled)
	}
	return scaled
}

// halve averages each 2×2 block of an image with even sides into a pixel.
func halve(src *image.RGBA) *image.RGBA {
	w, h := src.Rect.Dx()/2, src.Rect.Dy()/2
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		top := src.Pix[(2*y)*src.Stride:]
		bottom := src.Pix[(2*y+1)*src.Stride:]
		row := dst.Pix[y*dst.Stride:]
		for x := 0; x < w; x++ {
			for c := 0; c < 4; c++ {
				i := 8*x + c
				sum := uint32(top[i]) + uint32(top[i+4]) + uint32(bottom[i]) + uint32(bottom[i+4])
				row[4*x+c] = uint8((sum + 2) / 4)
			}
		}
	}
	return dst
}
