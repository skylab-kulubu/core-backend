package media_test

import (
	"encoding/json"
	"errors"
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

func TestCatalogue_PublicRasterImagesAreReencoded(t *testing.T) {
	t.Parallel()
	data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
		delete(purposes["event_cover"], "image")
	})
	if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingPublicRaster) {
		t.Fatalf("public raster purpose without re-encoding: err = %v, want %v", err, media.ErrCeilingPublicRaster)
	}
}

func TestCatalogue_SVGIsNeverStoredAsSVG(t *testing.T) {
	t.Parallel()
	data := reviewedCatalogueWith(t, func(purposes purposeEntries) {
		purposes["cms_image"]["types"] = append(purposes["cms_image"]["types"].([]any), "image/svg+xml")
	})
	if _, err := media.ParseCatalogue(data); !errors.Is(err, media.ErrCeilingSVG) {
		t.Fatalf("purpose keeping SVG as SVG: err = %v, want %v", err, media.ErrCeilingSVG)
	}

	rasterized := reviewedCatalogueWith(t, func(purposes purposeEntries) {
		purposes["cms_image"]["types"] = append(purposes["cms_image"]["types"].([]any), "image/svg+xml")
		purposes["cms_image"]["image"].(map[string]any)["rasterize_svg"] = true
	})
	if _, err := media.ParseCatalogue(rasterized); err != nil {
		t.Fatalf("purpose rasterizing SVG to PNG: %v", err)
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
			p["event_cover"]["image"].(map[string]any)["variants"] = map[string]any{"poster": 3000}
		},
		"name that is not a slug": func(p purposeEntries) { p["Event Cover"] = p["event_cover"] },
		"no legacy purpose":       func(p purposeEntries) { delete(p, "legacy") },
		"no profile picture":      func(p purposeEntries) { delete(p, "profile_picture") },
		"legacy rules on another": func(p purposeEntries) { p["cms_file"]["legacy_rules"] = true },
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
