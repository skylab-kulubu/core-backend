// Command stubs holds the two harness-only processes of the local full erasure harness
// (account-erasure ticket 10). Neither ships anywhere.
//
//	-mode forms  A Forms stand-in that implements the Erasure command contract
//	             (docs/account-erasure-command.md) exactly: core-erasure token checks, the
//	             subject's account-access marker, the receipt, the advisory lock and the
//	             PII-free logs. It also serves one protected and one anonymous route for the
//	             old-JWT scenario. The real Forms endpoint is Fatih's (ticket 06); this stub is
//	             replaced by the real image when it ships.
//	-mode proxy  A transparent reverse proxy that can hold a response for a while, so a
//	             scenario can kill core after a service committed its erasure and before core
//	             saw the answer.
//
// Both write only the method, the path, the status and the duration of a request.
package main

import (
	"flag"
	"log"
)

func main() {
	mode := flag.String("mode", "forms", "forms or proxy")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)
	switch *mode {
	case "forms":
		runForms()
	case "proxy":
		runProxy()
	default:
		log.Fatalf("unknown mode %q", *mode)
	}
}
