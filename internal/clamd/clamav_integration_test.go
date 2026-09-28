package clamd_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/clamd"
	"github.com/skylab-kulubu/core-backend/internal/clamd/clamdtest"
)

// startClamAV runs a disposable clamd (clamdtest.Real) with a 1 MiB
// StreamMaxLength, AlertExceedsMax on (as the wizard sets it) and at most 2
// files per archive.
func startClamAV(t *testing.T) string {
	t.Helper()
	return clamdtest.Real(t, "CLAMD_CONF_StreamMaxLength=1M", "CLAMD_CONF_AlertExceedsMax=yes", "CLAMD_CONF_MaxFiles=2")
}

func TestRealClamAV(t *testing.T) {
	addr := startClamAV(t)
	client := clamd.New(addr)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	got, err := clamd.SelfTest(ctx, client)
	if err != nil {
		t.Fatalf("self-test: %v", err)
	}
	if !strings.HasPrefix(got.Version, "ClamAV 1.5.4/") || got.Signature == "" {
		t.Fatalf("self-test %+v", got)
	}

	clean, err := client.Scan(ctx, bytes.NewReader(bytes.Repeat([]byte("%PDF-1.7 harmless "), 1000)))
	if err != nil || clean.Infected() {
		t.Fatalf("clean file: %+v, %v", clean, err)
	}

	// Over StreamMaxLength (1 MiB here): clamd refuses the stream.
	if _, err := client.Scan(ctx, bytes.NewReader(make([]byte, 3<<20))); !errors.Is(err, clamd.ErrStreamTooLarge) {
		t.Fatalf("a file over StreamMaxLength: err = %v, want %v", err, clamd.ErrStreamTooLarge)
	}

	// An archive clamd cannot scan whole (more files than MaxFiles) is
	// reported under the Heuristics.Limits.Exceeded prefix the scan worker
	// rejects as too large to scan.
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	for i := range 4 {
		f, err := w.Create(fmt.Sprintf("part-%d.txt", i))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(f, "harmless %d", i)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	limited, err := client.Scan(ctx, &archive)
	if err != nil || !strings.HasPrefix(limited.Signature, "Heuristics.Limits.Exceeded.") {
		t.Fatalf("an archive over MaxFiles: %+v, %v", limited, err)
	}
}
