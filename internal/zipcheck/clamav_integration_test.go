package zipcheck_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/clamd"
	"github.com/skylab-kulubu/core-backend/internal/clamd/clamdtest"
	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

// Against a real clamd 1.5.4 with the test's limits (MaxFileSize 1M,
// MaxScanSize 4M, AlertExceedsMax on as the wizard sets it), each ZIP below
// carries the EICAR test file where clamd does not find it: clamd answers it
// clean. The check refuses every one. A ZIP the check passes is scanned
// whole: clamd finds the EICAR test file in it, nested ZIP included.
func TestRealClamAVPassesUnscannedWhatTheCheckRefuses(t *testing.T) {
	addr := clamdtest.Real(t, "CLAMD_CONF_MaxFileSize=1M", "CLAMD_CONF_MaxScanSize=4M",
		"CLAMD_CONF_StreamMaxLength=16M", "CLAMD_CONF_AlertExceedsMax=yes")
	client := clamd.New(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	eicar := clamd.EICAR()
	padded := append(append([]byte{}, eicar...), bytes.Repeat([]byte(" "), 2<<20)...)

	lying := build(t, file{name: "e.txt", data: padded, method: zip.Deflate, sized: true})
	l := layoutOf(t, lying)
	put32(lying, l.dir[0]+dirUSize, 100)
	put32(lying, l.local[0]+locUSize, 100)
	unknownMethod := build(t, file{name: "e.txt", data: eicar, method: zip.Store, sized: true})
	l = layoutOf(t, unknownMethod)
	put16(unknownMethod, l.dir[0]+dirMethod, 99)
	put16(unknownMethod, l.local[0]+locMethod, 99)

	for name, c := range map[string]struct {
		data []byte
		want error
	}{
		"a member past MaxFileSize":              {build(t, deflated("e.txt", padded)), zipcheck.ErrTooLarge},
		"a member whose headers say 100 bytes":   {lying, zipcheck.ErrInvalid},
		"a member with ZIP64 sizes":              {zip64Member(eicar, uint64(len(eicar))), zipcheck.ErrInvalid},
		"a member with an unknown method":        {unknownMethod, zipcheck.ErrInvalid},
		"a nested ZIP's member past MaxFileSize": {build(t, deflated("inner.zip", build(t, deflated("e.txt", padded)))), zipcheck.ErrTooLarge},
	} {
		verdict, err := client.Scan(ctx, bytes.NewReader(c.data))
		if err != nil || verdict.Infected() {
			t.Errorf("%s: clamd answered %+v, %v; the check was built on clamd passing it unscanned", name, verdict, err)
		}
		if err, _ := check(t, c.data, limits); !errors.Is(err, c.want) {
			t.Errorf("%s: check err = %v, want %v", name, err, c.want)
		}
	}

	scanned := build(t, deflated("e.txt", eicar), deflated("inner.zip", build(t, deflated("f.txt", eicar))))
	passes(t, scanned, limits)
	if verdict, err := client.Scan(ctx, bytes.NewReader(scanned)); err != nil || verdict.Signature != clamdtest.Signature {
		t.Fatalf("a ZIP the check passes: clamd answered %+v, %v", verdict, err)
	}
	nestedOnly := build(t, deflated("inner.zip", build(t, deflated("f.txt", eicar))))
	passes(t, nestedOnly, limits)
	if verdict, err := client.Scan(ctx, bytes.NewReader(nestedOnly)); err != nil || verdict.Signature != clamdtest.Signature {
		t.Fatalf("a nested ZIP the check passes: clamd answered %+v, %v", verdict, err)
	}
}
