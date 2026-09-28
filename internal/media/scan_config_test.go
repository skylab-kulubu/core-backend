package media_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
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
