package clamd_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/clamd"
)

// clamavImage is the ClamAV the deploy runs (clamav/clamav:1.5.4, see
// ops/wizards/media-clamav-wizard.sh in sky_lab_genel), in its Debian build,
// which also runs on arm64 machines. It carries a signature database, so
// clamd starts without reaching the internet.
const clamavImage = "clamav/clamav:1.5.4-debian"

// startClamAV runs a disposable clamd with a 1 MiB StreamMaxLength and
// removes it when the test ends. The image declares no volume and the
// container is --rm, so nothing is left behind. The test is skipped when
// Docker or the image cannot be used. clamd loads its database for up to a
// few minutes.
func startClamAV(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("core-clamav-test-%d", time.Now().UnixNano())
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", "127.0.0.1::3310",
		"-e", "CLAMAV_NO_FRESHCLAMD=true",
		"-e", "CLAMD_CONF_ConcurrentDatabaseReload=no",
		"-e", "CLAMD_CONF_StreamMaxLength=1M",
		clamavImage,
	)
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("docker run clamav: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() })

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
}
