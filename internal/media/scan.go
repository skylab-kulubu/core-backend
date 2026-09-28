package media

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

// Malware scan (media redesign ticket 12, ADR-0052, decision Q17): a Media
// of a purpose whose catalogue entry has scan: true is created scanning,
// and the scan worker (ScanWorker) streams its file to ClamAV's clamd. See
// "Malware scan" in docs/media-lifecycle.md.

// ClamAVAddrEnv names clamd's address, host:port on the internal network.
// Unset, core has no scanner: a purpose that needs a scan is refused
// (ErrPurposeNeedsScanner), as before the scanner existed.
const ClamAVAddrEnv = "MEDIA_CLAMAV_ADDR"

// clamd's archive limits, which the ZIP check (zipcheck, media redesign
// ticket 23) holds a ZIP within. Each must be what clamd's own clamd.conf
// says (the ClamAV wizard, ops/wizards/media-clamav-wizard.sh in
// sky_lab_genel): a ZIP within core's limits but beyond clamd's would be
// scanned in part without a report.
const (
	// ClamAVMaxFileEnv is clamd's MaxFileSize, in whole MiB.
	ClamAVMaxFileEnv = "MEDIA_CLAMAV_MAX_FILE_MIB"
	// ClamAVMaxScanEnv is clamd's MaxScanSize, in whole MiB.
	ClamAVMaxScanEnv = "MEDIA_CLAMAV_MAX_SCAN_MIB"
	// ClamAVMaxFilesEnv is clamd's MaxFiles.
	ClamAVMaxFilesEnv = "MEDIA_CLAMAV_MAX_FILES"
	// ClamAVMaxRecursionEnv is clamd's MaxRecursion.
	ClamAVMaxRecursionEnv = "MEDIA_CLAMAV_MAX_RECURSION"
)

// ScanLimits are clamd's archive limits (zipcheck.Limits).
type ScanLimits = zipcheck.Limits

// DefaultScanLimits are clamd's limits as the ClamAV wizard leaves them:
// MaxFileSize and MaxScanSize of 1024M, which it sets, and clamd's own
// MaxFiles (10000) and MaxRecursion (17), which it keeps.
var DefaultScanLimits = ScanLimits{MaxFileSize: 1024 << 20, MaxScanSize: 1024 << 20, MaxFiles: 10000, MaxRecursion: 17}

// ScanConfig is where clamd is, and its limits.
type ScanConfig struct {
	// Addr is clamd's host:port; empty when no scanner is configured.
	Addr string
	// Limits are clamd's archive limits; DefaultScanLimits unless set.
	Limits ScanLimits
}

// Enabled reports whether a scanner is configured.
func (c ScanConfig) Enabled() bool { return c.Addr != "" }

// ScanConfigFromEnv reads MEDIA_CLAMAV_ADDR and, when it is set, clamd's
// limits (MEDIA_CLAMAV_MAX_FILE_MIB, MEDIA_CLAMAV_MAX_SCAN_MIB,
// MEDIA_CLAMAV_MAX_FILES, MEDIA_CLAMAV_MAX_RECURSION). An address that is not
// host:port with a port from 1 to 65535, or a limit that is not a whole
// number in its range, stops core at startup.
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
	limits := DefaultScanLimits
	for _, setting := range []struct {
		name     string
		low, top int64
		set      func(int64)
	}{
		// clamd takes MaxFileSize and MaxScanSize under 4 GiB.
		{ClamAVMaxFileEnv, 1, 4095, func(n int64) { limits.MaxFileSize = n << 20 }},
		{ClamAVMaxScanEnv, 1, 4095, func(n int64) { limits.MaxScanSize = n << 20 }},
		{ClamAVMaxFilesEnv, 1, zipcheck.MaxEntries, func(n int64) { limits.MaxFiles = int(n) }},
		{ClamAVMaxRecursionEnv, 2, 255, func(n int64) { limits.MaxRecursion = int(n) }},
	} {
		value := strings.TrimSpace(getenv(setting.name))
		if value == "" {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < setting.low || n > setting.top {
			return ScanConfig{}, fmt.Errorf("%s must be a whole number from %d to %d, as clamd.conf has it", setting.name, setting.low, setting.top)
		}
		setting.set(n)
	}
	return ScanConfig{Addr: raw, Limits: limits}, nil
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

// ScanRefusal refuses to open a Media because of its malware scan.
// errors.Is matches ErrMediaScanning or ErrMediaRejected.
type ScanRefusal struct {
	Err error
	// Result is why a rejected Media was rejected.
	Result ScanResult
}

func (r *ScanRefusal) Error() string { return r.Err.Error() }

func (r *ScanRefusal) Unwrap() error { return r.Err }

// openable is nil for a Media whose file may be opened, or the scan's
// refusal.
func (m Media) openable() error {
	switch m.Status {
	case StatusScanning:
		return &ScanRefusal{Err: ErrMediaScanning}
	case StatusRejected:
		return &ScanRefusal{Err: ErrMediaRejected, Result: m.ScanResult}
	}
	return nil
}
