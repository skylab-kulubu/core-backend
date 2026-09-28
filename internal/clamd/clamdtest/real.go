package clamdtest

import (
	"context"
	"fmt"
	"os/exec"
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
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("core-clamav-test-%d", time.Now().UnixNano())
	args := []string{"run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::3310",
		"-e", "CLAMAV_NO_FRESHCLAMD=true", "-e", "CLAMD_CONF_ConcurrentDatabaseReload=no"}
	for _, setting := range conf {
		args = append(args, "-e", setting)
	}
	if out, err := exec.Command("docker", append(args, RealImage)...).CombinedOutput(); err != nil {
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
