package media_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/skylab-kulubu/core-backend/config"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

const docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

type purposeEntries map[string]map[string]any

// reviewedCatalogueWith is the reviewed catalogue after one change, so each
// test breaks exactly one rule of an otherwise valid file.
func reviewedCatalogueWith(t *testing.T, change func(purposes purposeEntries)) []byte {
	t.Helper()
	var file map[string]purposeEntries
	if err := json.Unmarshal(config.MediaPurposes, &file); err != nil {
		t.Fatal(err)
	}
	change(file["purposes"])
	out, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCatalogue_ReviewedFileLoadsWithEveryPurpose(t *testing.T) {
	t.Parallel()
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatalf("reviewed catalogue: %v", err)
	}
	for _, name := range []string{
		"profile_picture", "event_cover", "event_gallery", "certificate_asset", "cms_image", "cms_file",
		"answer_file", "club_file", "answer_file_large", "video", "legacy",
	} {
		if _, ok := catalogue.Lookup(name); !ok {
			t.Errorf("catalogue has no %q purpose", name)
		}
	}
}

func TestCatalogue_PublicPurposeNamesOnlyRasterPDFOrMP4(t *testing.T) {
	t.Parallel()
	data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
		purposes["cms_file"]["types"] = []any{"application/pdf", docxType}
	})
	if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingPublicType) {
		t.Fatalf("public purpose accepting DOCX: err = %v, want %v", err, media.ErrCeilingPublicType)
	}
}

// Core attaches club files and videos (decision C1): an Event lists them as
// its files and videos. So a club file or video can be uploaded (once the
// runtime gates allow it) instead of being refused as nothing could attach
// it.
func TestCatalogue_CoreAttachesClubFilesAndVideos(t *testing.T) {
	t.Parallel()
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"club_file", "video"} {
		purpose, _ := catalogue.Lookup(name)
		if purpose.Attach != media.AttachCore || purpose.Service != "" || purpose.OwningProduct() != "core" {
			t.Errorf("%s is attached by %q (service %q), want core", name, purpose.Attach, purpose.Service)
		}
		if purpose.Transport != media.TransportDirect || purpose.Visibility != media.VisibilityPublic {
			t.Errorf("%s: transport %s, visibility %s", name, purpose.Transport, purpose.Visibility)
		}
	}
}

// A club file may be a ZIP (decision D6): a download-only public type, never
// served inline, and only where the catalogue names it.
func TestCatalogue_ClubFilesAcceptZIPAsADownload(t *testing.T) {
	t.Parallel()
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	club, _ := catalogue.Lookup("club_file")
	if !slices.Contains(club.Types, "application/zip") || !slices.Contains(club.Types, "application/pdf") {
		t.Fatalf("club_file types %v, want PDF and ZIP", club.Types)
	}
	for _, purpose := range []string{"cms_file", "video"} {
		data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
			purposes[purpose]["types"] = append(purposes[purpose]["types"].([]any), "application/zip")
		})
		if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingZIP) {
			t.Errorf("public %s naming ZIP: err = %v, want %v", purpose, err, media.ErrCeilingZIP)
		}
	}
}

// Only the video purpose names MP4 publicly: it is the one served inline, to
// play (ServingMetadataFor). A club file or a CMS document naming MP4 is
// refused at startup.
func TestCatalogue_OnlyVideosAcceptMP4Publicly(t *testing.T) {
	t.Parallel()
	for _, purpose := range []string{"club_file", "cms_file"} {
		data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
			purposes[purpose]["types"] = append(purposes[purpose]["types"].([]any), "video/mp4")
		})
		if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingMP4) {
			t.Errorf("public %s naming MP4: err = %v, want %v", purpose, err, media.ErrCeilingMP4)
		}
	}
}

// A video needs no malware scan. Its served key follows its type
// (videos/<uuid>.mp4, so players and saved copies know it), while a scanned
// file is served at files/<Media id>, which every purge of a held Media
// finds by the id alone. A scanned video would be served without its
// extension, so the catalogue may not ask for one, even within what clamd
// scans.
func TestCatalogue_VideosNeedNoScan(t *testing.T) {
	t.Parallel()
	data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
		purposes["video"]["scan"] = true
		purposes["video"]["max_mib"] = media.MaxScanBytes >> 20
	})
	if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingVideoScan) {
		t.Fatalf("a scanned video: err = %v, want %v", err, media.ErrCeilingVideoScan)
	}
}

// Core never receives a Direct upload's bytes; it reads only their start.
// So a Direct upload purpose names only types that start the same way every
// time and that core keeps as they came: PDF, ZIP and MP4. An image would
// reach the CDN without the re-encoding every public image gets, and a DOCX
// is told from a ZIP only by reading all of it.
func TestCatalogue_DirectUploadPurposesNameOnlyTypesTheirFirstBytesProve(t *testing.T) {
	t.Parallel()
	for purpose, extra := range map[string]string{"video": "image/png", "answer_file_large": docxType, "club_file": "image/gif"} {
		data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
			purposes[purpose]["types"] = append(purposes[purpose]["types"].([]any), extra)
			if extra == "image/png" || extra == "image/gif" {
				purposes[purpose]["image"] = map[string]any{"reencode": true}
			}
		})
		if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingDirectType) {
			t.Errorf("%s naming %s: err = %v, want %v", purpose, extra, err, media.ErrCeilingDirectType)
		}
	}
}

func TestCatalogue_PublicRasterImagesAreReencoded(t *testing.T) {
	t.Parallel()
	data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
		delete(purposes["event_cover"], "image")
	})
	if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingPublicRaster) {
		t.Fatalf("public raster purpose without re-encoding: err = %v, want %v", err, media.ErrCeilingPublicRaster)
	}
}

// SVG is served only as a sanitized download from the CDN: a private
// purpose (an Answer file, a certificate asset) cannot list it.
func TestCatalogue_OnlyCMSImagesAndEventPicturesAcceptSVG(t *testing.T) {
	t.Parallel()
	// And among the public ones, only CMS images and Event pictures: SVG is
	// never a profile picture.
	for _, purpose := range []string{"answer_file", "certificate_asset", "profile_picture"} {
		data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
			purposes[purpose]["types"] = append(purposes[purpose]["types"].([]any), "image/svg+xml")
		})
		if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingSVG) {
			t.Fatalf("%s naming SVG: err = %v, want %v", purpose, err, media.ErrCeilingSVG)
		}
	}
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	for purpose, want := range map[string]bool{"cms_image": true, "event_cover": true, "event_gallery": true, "profile_picture": false, "answer_file": false} {
		p, _ := catalogue.Lookup(purpose)
		if got := slices.Contains(p.Types, "image/svg+xml"); got != want {
			t.Errorf("%s lists SVG: %v, want %v", purpose, got, want)
		}
	}
}

// SVG is refused for a private purpose by its visibility, not only by its
// name: even a purpose that may list SVG cannot once it is private.
func TestCatalogue_NoPrivatePurposeAcceptsSVG(t *testing.T) {
	t.Parallel()
	data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
		purposes["cms_image"]["visibility"] = "private"
		purposes["cms_image"]["encrypted"] = true
		purposes["cms_image"]["types"] = []any{"image/svg+xml"}
	})
	if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingSVG) {
		t.Fatalf("a private purpose naming SVG: err = %v, want %v", err, media.ErrCeilingSVG)
	}
}

// A private raster image is re-encoded before it is encrypted, as a public
// one is before it is served: no uploaded image bytes are stored as they
// came.
func TestCatalogue_PrivateRasterPurposeDeclaresReencoding(t *testing.T) {
	t.Parallel()
	data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
		delete(purposes["certificate_asset"], "image")
	})
	if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingPrivateRaster) {
		t.Fatalf("private raster purpose without re-encoding: err = %v, want %v", err, media.ErrCeilingPrivateRaster)
	}
}

func TestCatalogue_SizesStayUnderTheGlobalMaximums(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		purpose string
		maxMiB  int
	}{
		{"cms_file", 21}, // single-step: 20 MiB
		{"video", 2049},  // Direct upload: 2 GiB
	} {
		data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
			purposes[tc.purpose]["max_mib"] = tc.maxMiB
		})
		if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingSize) {
			t.Errorf("%s at %d MiB: err = %v, want %v", tc.purpose, tc.maxMiB, err, media.ErrCeilingSize)
		}
	}
}

func TestCatalogue_PrivatePurposesAreEncrypted(t *testing.T) {
	t.Parallel()
	data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
		purposes["answer_file"]["encrypted"] = false
	})
	if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingPrivate) {
		t.Fatalf("private purpose stored in the clear: err = %v, want %v", err, media.ErrCeilingPrivate)
	}
}

func TestCatalogue_ImagesStayWithinTheDimensionCap(t *testing.T) {
	t.Parallel()
	data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
		purposes["event_gallery"]["image"].(map[string]any)["max_dimension"] = 4096
	})
	if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingImageDimension) {
		t.Fatalf("purpose keeping 4096 px images: err = %v, want %v", err, media.ErrCeilingImageDimension)
	}
}

func TestCatalogue_RefusesMalformedEntries(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(purposes purposeEntries){
		"misspelled field":        func(p purposeEntries) { p["cms_file"]["max_mb"] = 5 },
		"unknown uploader":        func(p purposeEntries) { p["cms_file"]["upload"] = "anyone" },
		"unknown type":            func(p purposeEntries) { p["answer_file"]["types"] = []any{"text/html"} },
		"repeated type":           func(p purposeEntries) { p["cms_file"]["types"] = []any{"application/pdf", "application/pdf"} },
		"no types":                func(p purposeEntries) { p["cms_file"]["types"] = []any{} },
		"no maximum size":         func(p purposeEntries) { delete(p["cms_file"], "max_mib") },
		"unknown visibility":      func(p purposeEntries) { p["cms_file"]["visibility"] = "internal" },
		"public and encrypted":    func(p purposeEntries) { p["cms_file"]["encrypted"] = true },
		"unknown transport":       func(p purposeEntries) { p["cms_file"]["transport"] = "courier" },
		"unknown attacher":        func(p purposeEntries) { p["cms_file"]["attach"] = "anyone" },
		"no attacher":             func(p purposeEntries) { delete(p["cms_file"], "attach") },
		"unknown service":         func(p purposeEntries) { p["cms_file"]["service"] = "arge" },
		"service on core purpose": func(p purposeEntries) { p["event_cover"]["service"] = "cms" },
		"role of another service": func(p purposeEntries) { p["cms_image"]["service"] = "forms" },
		"no CMS image":            func(p purposeEntries) { delete(p, "cms_image") },
		"no Answer file":          func(p purposeEntries) { delete(p, "answer_file") },
		"unreadable pending TTL":  func(p purposeEntries) { p["cms_file"]["pending_ttl"] = "tomorrow" },
		"pending that never ends": func(p purposeEntries) { p["cms_file"]["pending_ttl"] = "none" },
		"variant above the image": func(p purposeEntries) {
			p["event_cover"]["image"].(map[string]any)["sizes"] = map[string]any{"page": 3000}
		},
		// Size names are what clients ask for; a new one is a code change.
		"size clients cannot name": func(p purposeEntries) {
			p["event_cover"]["image"].(map[string]any)["sizes"] = map[string]any{"poster": 800}
		},
		"name that is not a slug": func(p purposeEntries) { p["Event Cover"] = p["event_cover"] },
		"SVG rasterizing, no more": func(p purposeEntries) {
			p["cms_image"]["image"].(map[string]any)["rasterize_svg"] = true
		},
		"no legacy purpose":       func(p purposeEntries) { delete(p, "legacy") },
		"no profile picture":      func(p purposeEntries) { delete(p, "profile_picture") },
		"legacy rules on another": func(p purposeEntries) { p["cms_file"]["legacy_rules"] = true },
		// An Event's files and videos are core's roles: core must attach
		// club files and videos.
		"core role's purpose attached by a service": func(p purposeEntries) {
			p["club_file"]["attach"] = "service"
			p["club_file"]["service"] = "cms"
		},
		"core role's purpose attached by nobody": func(p purposeEntries) {
			p["video"]["attach"] = "service"
		},
		"no club file": func(p purposeEntries) {
			delete(p, "club_file")
		},
		"no video": func(p purposeEntries) {
			delete(p, "video")
		},
	} {
		data := reviewedCatalogueWith(t, change)
		if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCatalogueInvalid) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrCatalogueInvalid)
		}
	}
}

func TestCatalogue_RefusesContentAfterTheCatalogue(t *testing.T) {
	t.Parallel()
	for _, trailing := range []string{`{"purposes": {}}`, "x"} {
		data := append(append([]byte{}, config.MediaPurposes...), trailing...)
		if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCatalogueInvalid) {
			t.Errorf("catalogue followed by %q: err = %v, want %v", trailing, err, media.ErrCatalogueInvalid)
		}
	}
}

// A public file that needs a malware scan must not be served before it is
// clean. Core holds a Direct upload's file under pending/ until its scan
// ends; a single-step upload would be written straight to its served key.
func TestCatalogue_PublicPurposeThatNeedsAScanIsADirectUpload(t *testing.T) {
	t.Parallel()
	data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
		purposes["cms_file"]["scan"] = true
	})
	if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingPublicScan) {
		t.Fatalf("public single-step purpose with a scan: err = %v, want %v", err, media.ErrCeilingPublicScan)
	}
}

// A purpose that needs a scan stays within what clamd takes in one stream
// (MaxScanBytes, the StreamMaxLength the ClamAV wizard sets): a larger one
// would be rejected as too large to scan every time.
func TestCatalogue_PurposeThatNeedsAScanFitsTheScanner(t *testing.T) {
	t.Parallel()
	for _, purpose := range []string{"club_file", "answer_file_large"} {
		data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
			purposes[purpose]["max_mib"] = media.MaxScanBytes>>20 + 1
		})
		if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingScanSize) {
			t.Errorf("%s above the scanner's limit: err = %v, want %v", purpose, err, media.ErrCeilingScanSize)
		}
	}
	// video needs no scan: it may be larger.
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	if video, _ := catalogue.Lookup("video"); video.Scan || video.MaxBytes <= media.MaxScanBytes {
		t.Fatalf("video scan %v, max %d", video.Scan, video.MaxBytes)
	}
}
