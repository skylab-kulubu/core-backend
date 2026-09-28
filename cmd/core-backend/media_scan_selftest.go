package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/clamd"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// mediaScanSelfTestCommandName checks the malware scanner instead of running
// the server: `core-backend media-scan-selftest [-addr host:port]`. It sends
// the EICAR test file to clamd at MEDIA_CLAMAV_ADDR (or -addr) and exits 0
// only when clamd reports it FOUND. The deploy check runs it inside core's
// container (ops/wizards/media-clamav-wizard.sh in sky_lab_genel), so it
// also proves core reaches clamd over the internal network.
const mediaScanSelfTestCommandName = "media-scan-selftest"

// mediaScanSelfTestTimeout bounds the whole check; clamd answers the test
// file at once once its database is loaded.
const mediaScanSelfTestTimeout = 30 * time.Second

// runMediaScanSelfTest exits 0 when clamd reports the EICAR test file, 1
// when it does not or cannot be reached, and 2 without a usable address.
func runMediaScanSelfTest(args []string, getenv func(string) string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet(mediaScanSelfTestCommandName, flag.ContinueOnError)
	flags.SetOutput(errOut)
	addr := flags.String("addr", "", "clamd's host:port; defaults to "+media.ClamAVAddrEnv)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *addr != "" {
		getenv = func(name string) string {
			if name == media.ClamAVAddrEnv {
				return *addr
			}
			return ""
		}
	}
	config, err := media.ScanConfigFromEnv(getenv)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", mediaScanSelfTestCommandName, err)
		return 2
	}
	if !config.Enabled() {
		fmt.Fprintf(errOut, "%s: set %s (or pass -addr host:port)\n", mediaScanSelfTestCommandName, media.ClamAVAddrEnv)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), mediaScanSelfTestTimeout)
	defer cancel()
	got, err := clamd.SelfTest(ctx, clamd.New(config.Addr))
	if err != nil {
		fmt.Fprintf(errOut, "%s: clamd at %s: %v\n", mediaScanSelfTestCommandName, config.Addr, err)
		return 1
	}
	fmt.Fprintf(out, "clamd at %s: %s\nEICAR test file: FOUND (%s)\n", config.Addr, got.Version, got.Signature)
	return 0
}
