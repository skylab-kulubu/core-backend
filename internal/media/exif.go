package media

import (
	"encoding/binary"
	"image"
	"image/draw"
)

// exifOrientationTag is the EXIF (TIFF) tag that says how a camera held the
// photo: 1 is upright, 2–8 are the mirrorings and quarter turns.
const exifOrientationTag = 0x0112

// jpegOrientation is the EXIF Orientation of a JPEG, read from its first
// EXIF segment before the image data. It is 1 (upright) when the JPEG has
// none or it cannot be read.
func jpegOrientation(data []byte) int {
	if !isJPEG(data) {
		return 1
	}
	pos := 2
	for pos+4 <= len(data) && data[pos] == 0xFF {
		marker := data[pos+1]
		if marker == 0xDA || marker == 0xD9 {
			break
		}
		size := int(binary.BigEndian.Uint16(data[pos+2:]))
		if size < 2 || pos+2+size > len(data) {
			break
		}
		payload := data[pos+4 : pos+2+size]
		if marker == 0xE1 {
			if o, ok := exifOrientation(payload); ok {
				return o
			}
		}
		pos += 2 + size
	}
	return 1
}

// exifOrientation reads the Orientation tag from an APP1 payload
// ("Exif\0\0" and a TIFF header), when it is there and valid.
func exifOrientation(payload []byte) (int, bool) {
	if len(payload) < 14 || string(payload[:6]) != "Exif\x00\x00" {
		return 0, false
	}
	tiff := payload[6:]
	var order binary.ByteOrder
	switch string(tiff[:4]) {
	case "II*\x00":
		order = binary.LittleEndian
	case "MM\x00*":
		order = binary.BigEndian
	default:
		return 0, false
	}
	ifd := int(order.Uint32(tiff[4:]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return 0, false
	}
	entries := int(order.Uint16(tiff[ifd:]))
	for i := 0; i < entries; i++ {
		entry := ifd + 2 + 12*i
		if entry+12 > len(tiff) {
			return 0, false
		}
		if order.Uint16(tiff[entry:]) != exifOrientationTag {
			continue
		}
		// A SHORT, count 1, stored in the first bytes of the value field.
		if order.Uint16(tiff[entry+2:]) != 3 || order.Uint32(tiff[entry+4:]) != 1 {
			return 0, false
		}
		o := int(order.Uint16(tiff[entry+8:]))
		if o < 1 || o > 8 {
			return 0, false
		}
		return o, true
	}
	return 0, false
}

// orient turns and mirrors img the way its EXIF Orientation says a viewer
// should show it, so that the pixels are upright and need no tag.
func orient(img image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return img
	}
	src, ok := img.(*image.RGBA)
	if !ok || src.Rect.Min != (image.Point{}) {
		src = image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
		draw.Draw(src, src.Rect, img, img.Bounds().Min, draw.Src)
	}
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch orientation {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			copy(dst.Pix[dst.PixOffset(x, y):dst.PixOffset(x, y)+4], src.Pix[src.PixOffset(sx, sy):src.PixOffset(sx, sy)+4])
		}
	}
	return dst
}
