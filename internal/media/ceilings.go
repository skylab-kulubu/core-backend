package media

import (
	"errors"
	"fmt"
	"slices"

	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// The hard ceilings of ADR-0052. No catalogue entry can loosen them: core
// refuses to start with a catalogue that breaks one.
var (
	// ErrCeilingPublicType: a public purpose accepts only raster images,
	// SVG (sanitized, served as a download), PDF, MP4 and, where the
	// catalogue names it, ZIP (ErrCeilingZIP).
	ErrCeilingPublicType = errors.New("media purpose catalogue: a public purpose accepts only raster images, SVG, PDF, MP4 and, for club files, ZIP")
	// ErrCeilingZIP: only club files (zipPurposes) accept ZIP publicly
	// (decision D6). A ZIP is download-only: the serving policy stores it
	// as application/octet-stream with Content-Disposition: attachment
	// (ServingMetadata), never inline. A private purpose may name ZIP: it
	// never reaches the CDN.
	ErrCeilingZIP = errors.New("media purpose catalogue: only club files accept ZIP publicly, as a download")
	// ErrCeilingMP4: only videos (videoPurposes) accept MP4 publicly. Their
	// MP4 is the one type served inline beside images and PDF, to play
	// (ServingMetadataFor): video/mp4, never a download. No other purpose
	// may have its MP4 played from the CDN.
	ErrCeilingMP4 = errors.New("media purpose catalogue: only videos accept MP4 publicly")
	// ErrCeilingVideoScan: a video (videoPurposes) needs no malware scan.
	// Its served key follows its type (directServedKey: videos/<uuid>.mp4),
	// while a scanned file is copied, once clean, to a served key that
	// follows its Media id alone (servedKeyOf: files/<Media id>), which every
	// purge of a held Media finds without knowing its purpose. A scanned
	// video would be served without its extension.
	ErrCeilingVideoScan = errors.New("media purpose catalogue: a video needs no malware scan")
	// ErrCeilingDirectType: a Direct upload purpose accepts only PDF, ZIP
	// and MP4 (directTypes). Core never receives a Direct upload's bytes; it
	// reads their start (detectDirectType) and keeps the object as it came.
	// These three prove their type in their first bytes. An image would
	// reach storage without the re-encoding every stored image gets, and a
	// DOCX is told from any other ZIP only by reading all of it.
	ErrCeilingDirectType = errors.New("media purpose catalogue: a Direct upload purpose accepts only PDF, ZIP and MP4")
	// ErrCeilingPublicRaster: a public purpose that accepts raster images
	// declares image.reencode, so that no uploaded image bytes reach the CDN
	// as they came: core decodes such an image and stores only its pixels,
	// encoded again (reencodeRaster; a GIF frame by frame, reencodeGIF).
	// The one exception is an animated WebP, which Go cannot encode: it is
	// kept as uploaded only after its structure is checked chunk by chunk
	// and every frame decodes (cleanAnimatedWebP), without its metadata.
	// Media uploaded without a purpose (legacy) are not a purpose's upload
	// and keep their stripped bytes until they fall to the strict rule.
	ErrCeilingPublicRaster = errors.New("media purpose catalogue: a public purpose declares re-encoding for raster images")
	// ErrCeilingSVG: only CMS images and Event pictures (svgPurposes)
	// accept SVG; never a profile picture or a private purpose. Core stores
	// an SVG only after sanitizing it (sanitizeSVG), under a key ending in
	// .svg, and serves it as a download (ServingMetadata): never inline,
	// and never for a purpose that does not list it.
	ErrCeilingSVG = errors.New("media purpose catalogue: only CMS images and Event pictures accept SVG")
	// ErrCeilingPrivateRaster: a private purpose that accepts raster images
	// declares re-encoding, as a public one does: the image is re-encoded
	// before it is encrypted, so no uploaded image bytes are stored as they
	// came.
	ErrCeilingPrivateRaster = errors.New("media purpose catalogue: a private purpose declares re-encoding for raster images")
	// ErrCeilingSize: a purpose's maximum stays under the global maximum of
	// its transport: MaxUploadBytes through core, MaxDirectUploadBytes by
	// Direct upload.
	ErrCeilingSize = errors.New("media purpose catalogue: maximum size above the global maximum of its transport")
	// ErrCeilingPrivate: a private purpose is always encrypted before it
	// reaches storage (KVKK cloud guidance, §3.4).
	ErrCeilingPrivate = errors.New("media purpose catalogue: a private purpose is encrypted")
	// ErrCeilingImageDimension: a re-encoded image is at most
	// MaxImageDimension pixels on its longer side; re-encoding scales a
	// larger one down to the purpose's max_dimension.
	ErrCeilingImageDimension = errors.New("media purpose catalogue: image dimension above the cap")
	// ErrCeilingPublicScan: a public purpose that needs a malware scan is a
	// Direct upload purpose. Its file must not be served before the scan
	// finds it clean: core holds a Direct upload's file under the pending
	// prefix, at a key only core knows, and copies it to its served key
	// once clean; a single-step upload is written straight to its served
	// key.
	ErrCeilingPublicScan = errors.New("media purpose catalogue: a public purpose that needs a malware scan is sent by Direct upload")
	// ErrCeilingScanSize: a purpose that needs a malware scan allows at
	// most MaxScanBytes, what clamd takes in one stream (its
	// StreamMaxLength, set by the ClamAV wizard). A larger file would be
	// rejected as too large to scan every time.
	ErrCeilingScanSize = errors.New("media purpose catalogue: a purpose that needs a malware scan allows at most what the scanner takes")
	// ErrCeilingVideoFrame: a video's frame (framePurposes) is an image core
	// makes itself from the video (FrameWorker), never an upload: no person
	// may upload one (service_only), core attaches it, and it is a public,
	// single-step raster image that is re-encoded with the rest. Opening it
	// to a person would let one set what core shows as a video's frame.
	ErrCeilingVideoFrame = errors.New("media purpose catalogue: a video frame is a public raster image core makes and attaches, which no person uploads")
)

// MaxScanBytes is the largest file the malware scanner takes: clamd's
// StreamMaxLength and MaxFileSize (1024M), which
// ops/wizards/media-clamav-wizard.sh in sky_lab_genel sets. Raising it
// means raising those first.
const MaxScanBytes = 1 << 30

// svgPurposes are the only purposes that may accept SVG.
var svgPurposes = []string{"cms_image", PurposeEventCover, PurposeEventGallery}

// zipPurposes are the only public purposes that may accept ZIP.
var zipPurposes = []string{PurposeClubFile}

// videoPurposes are the only public purposes that may accept MP4, and the
// only ones whose MP4 is served inline, to play.
var videoPurposes = []string{PurposeVideo}

// framePurposes are the images core makes itself from a video: no person
// uploads them.
var framePurposes = []string{PurposeVideoFrame}

// directTypes are the only types a Direct upload purpose may accept.
var directTypes = []string{pdfType, zipType, mp4Type}

// MaxImageDimension is the longest side, in pixels, of a re-encoded image,
// and of a purpose's image sizes.
const MaxImageDimension = 2560

// MaxDirectUploadBytes is the largest Media a Direct upload may carry.
const MaxDirectUploadBytes = 2 << 30

const (
	mp4Type  = "video/mp4"
	zipType  = "application/zip"
	docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

func checkCeilings(p Purpose) error {
	if slices.Contains(framePurposes, p.Name) && !coreMadeImage(p) {
		return fmt.Errorf("%s: %w", p.Name, ErrCeilingVideoFrame)
	}
	limit := int64(MaxUploadBytes)
	if p.Transport == TransportDirect {
		limit = MaxDirectUploadBytes
	}
	if p.MaxBytes > limit {
		return fmt.Errorf("%s allows %d bytes over %d: %w", p.Name, p.MaxBytes, limit, ErrCeilingSize)
	}
	if p.Transport == TransportDirect {
		for _, t := range p.Types {
			if !slices.Contains(directTypes, t) {
				return fmt.Errorf("%s names %s: %w", p.Name, t, ErrCeilingDirectType)
			}
		}
	}
	if p.Scan && p.MaxBytes > MaxScanBytes {
		return fmt.Errorf("%s allows %d bytes over %d: %w", p.Name, p.MaxBytes, MaxScanBytes, ErrCeilingScanSize)
	}
	if p.Scan && p.Visibility == VisibilityPublic && p.Transport != TransportDirect {
		return fmt.Errorf("%s: %w", p.Name, ErrCeilingPublicScan)
	}
	if p.Scan && slices.Contains(videoPurposes, p.Name) {
		return fmt.Errorf("%s: %w", p.Name, ErrCeilingVideoScan)
	}
	if p.Image.MaxDimension > MaxImageDimension {
		return fmt.Errorf("%s keeps %d px: %w", p.Name, p.Image.MaxDimension, ErrCeilingImageDimension)
	}
	if p.Visibility == VisibilityPrivate && !p.Encrypted {
		return fmt.Errorf("%s: %w", p.Name, ErrCeilingPrivate)
	}
	// SVG is never private, whatever the purpose is called.
	if slices.Contains(p.Types, svgType) && (p.Visibility == VisibilityPrivate || !slices.Contains(svgPurposes, p.Name)) {
		return fmt.Errorf("%s names %s: %w", p.Name, svgType, ErrCeilingSVG)
	}
	if p.Visibility == VisibilityPrivate && !p.Image.Reencode && slices.ContainsFunc(p.Types, isRasterType) {
		return fmt.Errorf("%s: %w", p.Name, ErrCeilingPrivateRaster)
	}
	if p.Visibility == VisibilityPublic {
		for _, t := range p.Types {
			if t == zipType {
				if !slices.Contains(zipPurposes, p.Name) {
					return fmt.Errorf("%s names %s: %w", p.Name, t, ErrCeilingZIP)
				}
				continue
			}
			if t == mp4Type && !slices.Contains(videoPurposes, p.Name) {
				return fmt.Errorf("%s names %s: %w", p.Name, t, ErrCeilingMP4)
			}
			if !isRasterType(t) && t != svgType && t != pdfType && t != mp4Type {
				return fmt.Errorf("%s names %s: %w", p.Name, t, ErrCeilingPublicType)
			}
			if isRasterType(t) && !p.Image.Reencode {
				return fmt.Errorf("%s names %s: %w", p.Name, t, ErrCeilingPublicRaster)
			}
		}
	}
	return nil
}

// coreMadeImage reports whether the purpose is one of core's own images: no
// person uploads it, core attaches it, and it is a public, single-step,
// re-encoded raster image.
func coreMadeImage(p Purpose) bool {
	if p.Uploader != authz.MediaUploaderServiceOnly || p.Attach != AttachCore || p.Visibility != VisibilityPublic ||
		p.Transport != TransportSingleStep || !p.Image.Reencode || len(p.Types) == 0 {
		return false
	}
	for _, t := range p.Types {
		if !isRasterType(t) {
			return false
		}
	}
	return true
}
