package media_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

// MEDIA_CLAMAV_ADDR names clamd on the internal network. Unset, core has
// no scanner; anything but host:port stops core at startup, naming the
// variable.
func TestScanConfigFromEnv(t *testing.T) {
	t.Parallel()
	env := func(value string) func(string) string {
		return func(name string) string {
			if name == media.ClamAVAddrEnv {
				return value
			}
			return ""
		}
	}
	for value, want := range map[string]string{"": "", "  ": "", "clamav-sandbox:3310": "clamav-sandbox:3310", " 10.0.1.7:3310 ": "10.0.1.7:3310", "[::1]:3310": "[::1]:3310"} {
		config, err := media.ScanConfigFromEnv(env(value))
		if err != nil || config.Addr != want || config.Enabled() != (want != "") {
			t.Errorf("%q: %+v, err %v", value, config, err)
		}
	}
	for _, value := range []string{"clamav", "clamav:", ":3310", "clamav:0", "clamav:65536", "clamav:tcp", "tcp://clamav:3310", "user@clamav:3310"} {
		if _, err := media.ScanConfigFromEnv(env(value)); err == nil || !strings.Contains(err.Error(), media.ClamAVAddrEnv) {
			t.Errorf("%q: err = %v, want a refusal naming %s", value, err, media.ClamAVAddrEnv)
		}
	}
}

// The ZIP check holds a ZIP within clamd's limits, which must be the ones
// the ClamAV wizard gives clamd: MaxFileSize and MaxScanSize of 1024 MiB,
// MaxFiles of 10000 and MaxRecursion of 17. Each can be set to match another
// clamd.conf. Core's own limit on what it keeps in memory to check an inner
// ZIP defaults to 64 MiB. A value out of range stops core at startup,
// naming the variable. Without clamd they are not read.
func TestScanConfigReadsClamdLimits(t *testing.T) {
	t.Parallel()
	env := func(values map[string]string) func(string) string {
		return func(name string) string {
			if name == media.ClamAVAddrEnv {
				return "clamav:3310"
			}
			return values[name]
		}
	}
	config, err := media.ScanConfigFromEnv(env(nil))
	want := zipcheck.Limits{MaxFileSize: 1024 << 20, MaxScanSize: 1024 << 20, MaxFiles: 10000, MaxRecursion: 17, MaxBuffer: 64 << 20}
	if err != nil || config.Limits != want || media.DefaultScanLimits != want {
		t.Fatalf("defaults %+v (DefaultScanLimits %+v), err %v; want %+v", config.Limits, media.DefaultScanLimits, err, want)
	}
	config, err = media.ScanConfigFromEnv(env(map[string]string{
		media.ClamAVMaxFileEnv: "512", media.ClamAVMaxScanEnv: " 2048 ", media.ClamAVMaxFilesEnv: "500", media.ClamAVMaxRecursionEnv: "5",
		media.ZIPCheckBufferEnv: "16",
	}))
	if want := (zipcheck.Limits{MaxFileSize: 512 << 20, MaxScanSize: 2048 << 20, MaxFiles: 500, MaxRecursion: 5, MaxBuffer: 16 << 20}); err != nil || config.Limits != want {
		t.Fatalf("set %+v, err %v; want %+v", config.Limits, err, want)
	}
	for name, values := range map[string][]string{
		media.ClamAVMaxFileEnv:      {"0", "-1", "4096", "1.5", "1G", "abc"},
		media.ClamAVMaxScanEnv:      {"0", "4096", "x"},
		media.ClamAVMaxFilesEnv:     {"0", "65536", "ten"},
		media.ClamAVMaxRecursionEnv: {"0", "1", "256", "-"},
		media.ZIPCheckBufferEnv:     {"0", "1025", "64M"},
	} {
		for _, value := range values {
			if _, err := media.ScanConfigFromEnv(env(map[string]string{name: value})); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: err = %v, want a refusal naming it", name, value, err)
			}
		}
	}
	off, err := media.ScanConfigFromEnv(func(name string) string {
		if name == media.ClamAVMaxFileEnv {
			return "abc"
		}
		return ""
	})
	if err != nil || off.Enabled() {
		t.Fatalf("no clamd: %+v, err %v", off, err)
	}
}

// A scan worker's limits are whole or not at all: left out, they are the
// wizard's (DefaultScanLimits); partly set, the worker is refused when core
// starts, never in each scan.
func TestNewScanWorkerRefusesPartialLimits(t *testing.T) {
	t.Parallel()
	if _, err := media.NewScanWorker(media.ScanWorkerConfig{}); err != nil {
		t.Fatalf("default limits: %v", err)
	}
	if _, err := media.NewScanWorker(media.ScanWorkerConfig{Limits: media.ScanLimits{MaxFileSize: 1 << 20}}); err == nil {
		t.Fatal("a worker with partial limits was made")
	}
}
