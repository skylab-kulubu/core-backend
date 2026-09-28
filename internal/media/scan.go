package media

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Malware scan (media redesign ticket 12, ADR-0052, decision Q17): a Media
// of a purpose whose catalogue entry has scan: true is created scanning,
// and the scan worker (ScanWorker) streams its file to ClamAV's clamd. See
// "Malware scan" in docs/media-lifecycle.md.

// ClamAVAddrEnv names clamd's address, host:port on the internal network.
// Unset, core has no scanner: a purpose that needs a scan is refused
// (ErrPurposeNeedsScanner), as before the scanner existed.
const ClamAVAddrEnv = "MEDIA_CLAMAV_ADDR"

// ScanConfig is where clamd is.
type ScanConfig struct {
	// Addr is clamd's host:port; empty when no scanner is configured.
	Addr string
}

// Enabled reports whether a scanner is configured.
func (c ScanConfig) Enabled() bool { return c.Addr != "" }

// ScanConfigFromEnv reads MEDIA_CLAMAV_ADDR. A value that is not host:port
// with a port from 1 to 65535 stops core at startup.
func ScanConfigFromEnv(getenv func(string) string) (ScanConfig, error) {
	raw := strings.TrimSpace(getenv(ClamAVAddrEnv))
	if raw == "" {
		return ScanConfig{}, nil
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil || host == "" || strings.ContainsAny(host, "/@") {
		return ScanConfig{}, fmt.Errorf("%s must be clamd's host:port (such as clamav:3310)", ClamAVAddrEnv)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return ScanConfig{}, fmt.Errorf("%s must name a port from 1 to 65535", ClamAVAddrEnv)
	}
	return ScanConfig{Addr: raw}, nil
}

// ScanQueue is the malware scan as uploads see it (ScanWorker). A service
// without one has no scanner, and its scan gate stays closed.
type ScanQueue interface {
	// Wake asks the scan to look for Media waiting for it now, rather
	// than at its next pass.
	Wake()
}

var (
	// ErrMediaScanning refuses to open a Media whose malware scan has not
	// ended yet: it may be attached, but it is opened only once clean.
	ErrMediaScanning = errors.New("media: the Media is waiting for its malware scan")
	// ErrMediaRejected refuses to open a Media the malware scan rejected:
	// its object is deleted.
	ErrMediaRejected = errors.New("media: the Media was rejected by its malware scan")
)

// openable is nil for a Media whose file may be opened, or the scan's
// refusal.
func (m Media) openable() error {
	switch m.Status {
	case StatusScanning:
		return ErrMediaScanning
	case StatusRejected:
		return ErrMediaRejected
	}
	return nil
}
