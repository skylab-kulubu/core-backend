package mediaframe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
)

// SampleVideo is a two-second MP4 of width by height pixels whose video
// samples are JPEG pictures (Motion JPEG, sample entry "jpeg", which ffmpeg
// reads): red for its first second, blue for its second. Its moov comes
// first (faststart). It is made here rather than stored, so core's
// self-test (core-backend media-frame-selftest) and the tests carry no
// binary fixture.
func SampleVideo(width, height int) ([]byte, error) {
	if width <= 0 || height <= 0 || width > 0xFFFF || height > 0xFFFF {
		return nil, errors.New("mediaframe: sample video size out of range")
	}
	var frames [][]byte
	for _, c := range []color.RGBA{{R: 220, G: 30, B: 30, A: 255}, {R: 30, G: 30, B: 220, A: 255}} {
		img := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
		var frame bytes.Buffer
		if err := jpeg.Encode(&frame, img, &jpeg.Options{Quality: 80}); err != nil {
			return nil, err
		}
		frames = append(frames, frame.Bytes())
	}
	const timescale, delta = 1000, 1000
	duration := uint32(delta * len(frames))
	ftyp := mp4Box("ftyp", []byte("isom"), be32(0x200), []byte("isomiso2mp41"))
	moov := func(chunkOffset uint32) []byte {
		sizes := be32(0) // every sample's own size
		sizes = append(sizes, be32(uint32(len(frames)))...)
		for _, f := range frames {
			sizes = append(sizes, be32(uint32(len(f)))...)
		}
		compressor := make([]byte, 32)
		compressor[0] = byte(copy(compressor[1:], "Photo - JPEG"))
		sampleEntry := mp4Box("jpeg",
			make([]byte, 6), be16(1), // reserved, data_reference_index
			make([]byte, 16), // pre_defined, reserved, pre_defined
			be16(uint16(width)), be16(uint16(height)),
			be32(0x00480000), be32(0x00480000), // 72 dpi
			be32(0), be16(1), compressor, be16(0x0018), be16(0xFFFF))
		return mp4Box("moov",
			mp4FullBox("mvhd", 0, 0, be32(0), be32(0), be32(timescale), be32(duration),
				be32(0x00010000), be16(0x0100), make([]byte, 10), unityMatrix(), make([]byte, 24), be32(2)),
			mp4Box("trak",
				mp4FullBox("tkhd", 0, 3, be32(0), be32(0), be32(1), be32(0), be32(duration), make([]byte, 8),
					be16(0), be16(0), be16(0), be16(0), unityMatrix(), be32(uint32(width)<<16), be32(uint32(height)<<16)),
				mp4Box("mdia",
					mp4FullBox("mdhd", 0, 0, be32(0), be32(0), be32(timescale), be32(duration), be16(0x55C4), be16(0)),
					mp4FullBox("hdlr", 0, 0, be32(0), []byte("vide"), make([]byte, 12), []byte("VideoHandler\x00")),
					mp4Box("minf",
						mp4FullBox("vmhd", 0, 1, make([]byte, 8)),
						mp4Box("dinf", mp4FullBox("dref", 0, 0, be32(1), mp4FullBox("url ", 0, 1))),
						mp4Box("stbl",
							mp4FullBox("stsd", 0, 0, be32(1), sampleEntry),
							mp4FullBox("stts", 0, 0, be32(1), be32(uint32(len(frames))), be32(delta)),
							mp4FullBox("stsc", 0, 0, be32(1), be32(1), be32(uint32(len(frames))), be32(1)),
							mp4FullBox("stsz", 0, 0, sizes),
							mp4FullBox("stco", 0, 0, be32(1), be32(chunkOffset)),
						)))))
	}
	// The moov's size does not depend on the offset it holds.
	offset := len(ftyp) + len(moov(0)) + 8
	return bytes.Join([][]byte{ftyp, moov(uint32(offset)), mp4Box("mdat", frames...)}, nil), nil
}

func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

func mp4Box(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	return append(append(be32(uint32(8+len(body))), typ...), body...)
}

func mp4FullBox(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	return mp4Box(typ, append([]byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}, bytes.Join(payload, nil)...))
}

// unityMatrix is the identity transformation matrix of mvhd and tkhd.
func unityMatrix() []byte {
	return bytes.Join([][]byte{be32(0x00010000), be32(0), be32(0), be32(0), be32(0x00010000), be32(0), be32(0), be32(0), be32(0x40000000)}, nil)
}
