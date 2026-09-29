package mediaframe

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// The allowlist comes from the environment: bare host names, or the https
// address of the storage endpoint (core's R2_ENDPOINT), taken down to its
// host. Anything that would widen it is refused at startup.
func TestParseAllowedHosts(t *testing.T) {
	t.Parallel()
	got, err := ParseAllowedHosts(" Abc123.eu.R2.cloudflarestorage.com , https://def456.r2.cloudflarestorage.com/ ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"abc123.eu.r2.cloudflarestorage.com", "def456.r2.cloudflarestorage.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("hosts %v, want %v", got, want)
	}
	for _, bad := range []string{
		"",
		" , ",
		"http://abc.r2.cloudflarestorage.com",
		"https://abc.r2.cloudflarestorage.com/bucket",
		"https://user@abc.r2.cloudflarestorage.com",
		"abc.r2.cloudflarestorage.com:8443",
		"*.r2.cloudflarestorage.com",
		"127.0.0.1",
		"[::1]",
		"::1",
		"localhost:443/x",
		"exa mple.com",
	} {
		if _, err := ParseAllowedHosts(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

// A frame is read only from an https address on an allowed host, named by
// its name: never plain http, another host, an IP address, credentials in
// the address, or another port.
func TestCheckURLRefusesAnythingButHTTPSOnAnAllowedHost(t *testing.T) {
	t.Parallel()
	allowed := []string{"bucket.r2.test"}
	good := "https://bucket.r2.test/videos/a.mp4?X-Amz-Signature=abc"
	if u, err := checkURL(good, allowed); err != nil || u.String() != good {
		t.Fatalf("an allowed address: %v, %v", u, err)
	}
	for _, ok := range []string{"https://BUCKET.r2.test/v.mp4", "https://bucket.r2.test:443/v.mp4"} {
		if _, err := checkURL(ok, allowed); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://bucket.r2.test/v.mp4",
		"ftp://bucket.r2.test/v.mp4",
		"file:///etc/passwd",
		"https://other.r2.test/v.mp4",
		"https://bucket.r2.test.evil.test/v.mp4",
		"https://evil.test/bucket.r2.test/v.mp4",
		"https://127.0.0.1/v.mp4",
		"https://[::1]/v.mp4",
		"https://169.254.169.254/latest/meta-data",
		"https://2130706433/v.mp4",
		"https://user:pass@bucket.r2.test/v.mp4",
		"https://bucket.r2.test:8443/v.mp4",
		"//bucket.r2.test/v.mp4",
		"bucket.r2.test/v.mp4",
		"https:bucket.r2.test/v.mp4",
		"",
		"https://bucket.r2.test/" + strings.Repeat("a", maxURLBytes),
	} {
		if _, err := checkURL(bad, allowed); !errors.Is(err, ErrURLNotAllowed) {
			t.Errorf("%q: %v, want ErrURLNotAllowed", bad, err)
		}
	}
}
