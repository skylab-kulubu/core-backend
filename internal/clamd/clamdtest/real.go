package clamdtest

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/clamd"
)

// RealImage is the ClamAV the deploy runs (clamav/clamav:1.5.4, see
// ops/wizards/media-clamav-wizard.sh in sky_lab_genel), in its Debian build,
// which also runs on arm64 machines. It carries a signature database, so
// clamd starts without reaching the internet.
const RealImage = "clamav/clamav:1.5.4-debian"

// Real runs a disposable clamd with ConcurrentDatabaseReload off (as the
// wizard sets it) and the given clamd.conf settings as the image takes
// them (CLAMD_CONF_<Setting>=<value>), and removes it when the test ends.
// It answers clamd's address. The image declares no volume and the
// container is --rm, so nothing is left behind. The test is skipped in
// short mode and when Docker or the image cannot be used. clamd loads its
// database for up to a few minutes.
func Real(t testing.TB, conf ...string) string {
	t.Helper()
	return real(t, "", conf)
}

// Marker is a string the test signature RealWithMarker loads matches
// anywhere in a scanned file, reported as MarkerSignature. Unlike the EICAR
// test file, which clamd reports only as a whole file, it tells whether clamd
// read the part of a file it sits in.
const (
	Marker = "CORE_ZIPCHECK_TEST_MARKER_7f3a"
	// MarkerSignature is how clamd names what it found: the signature's
	// name, marked as not from its official database.
	MarkerSignature = markerName + ".UNOFFICIAL"
	markerName      = "Core.Test.Marker"
)

// RealWithMarker is Real with one test signature loaded besides the
// image's: Marker, anywhere in any file (MarkerSignature).
func RealWithMarker(t testing.TB, conf ...string) string {
	t.Helper()
	return real(t, markerName+":0:*:"+hex.EncodeToString([]byte(Marker))+"\n", conf)
}

func real(t testing.TB, ndb string, conf []string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("core-clamav-test-%d", time.Now().UnixNano())
	args := []string{"create", "--rm", "--name", name, "-p", "127.0.0.1::3310",
		"-e", "CLAMAV_NO_FRESHCLAMD=true", "-e", "CLAMD_CONF_ConcurrentDatabaseReload=no"}
	for _, setting := range conf {
		args = append(args, "-e", setting)
	}
	if out, err := exec.Command("docker", append(args, RealImage)...).CombinedOutput(); err != nil {
		t.Skipf("docker create clamav: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() })
	// A signature file is copied in before clamd starts: the image's start
	// script takes ownership of the database directory, which a bind mount
	// would refuse.
	if ndb != "" {
		path := filepath.Join(t.TempDir(), "core-test.ndb")
		if err := os.WriteFile(path, []byte(ndb), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("docker", "cp", path, name+":/var/lib/clamav/core-test.ndb").CombinedOutput(); err != nil {
			t.Fatalf("docker cp the test signature: %v %s", err, out)
		}
	}
	if out, err := exec.Command("docker", "start", name).CombinedOutput(); err != nil {
		t.Skipf("docker start clamav: %v %s", err, out)
	}

	deadline := time.Now().Add(4 * time.Minute)
	for {
		out, err := exec.Command("docker", "port", name, "3310/tcp").CombinedOutput()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			addr := strings.TrimSpace(strings.Split(string(out), "\n")[0])
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err = clamd.New(addr).Ping(ctx)
			cancel()
			if err == nil {
				return addr
			}
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", "--tail", "20", name).CombinedOutput()
			t.Fatalf("clamd never answered: %v\n%s", err, logs)
		}
		time.Sleep(time.Second)
	}
}
