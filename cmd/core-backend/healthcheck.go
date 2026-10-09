package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// healthcheckCommandName is the container's health check (Dockerfile
// HEALTHCHECK, Swarm's Health Check): `core-backend healthcheck` asks this
// container's own core for GET /v1/ready?gate=skip, that is whether it is not
// shutting down and its database answers, leaving out the account access
// gate's Redis (docs/health-and-shutdown.md). The image needs no curl or wget
// for it.
const healthcheckCommandName = "healthcheck"

// healthcheckTimeout stays under Swarm's timeout for the check (5 s) and
// over readiness's own database ping (health.DefaultPingTimeout, 2 s).
const healthcheckTimeout = 3 * time.Second

// healthcheckPath is readiness without the account access gate.
const healthcheckPath = "/v1/ready?gate=skip"

// runHealthcheck exits 0 when core on PORT (8080 unset) answers
// /v1/ready?gate=skip with 2xx, and 1 otherwise; Docker reserves 2. It writes why to stderr,
// which Docker keeps with the check's result (docker inspect).
func runHealthcheck(args []string, getenv func(string) string, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "usage: core-backend %s (no arguments; PORT is read from the environment)\n", healthcheckCommandName)
		return 1
	}
	port := strings.TrimSpace(getenv("PORT"))
	if port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: healthcheckTimeout}
	response, err := client.Get("http://127.0.0.1:" + port + healthcheckPath)
	if err != nil {
		fmt.Fprintf(stderr, "core-backend healthcheck: %v\n", err)
		return 1
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		fmt.Fprintf(stderr, "core-backend healthcheck: %s answered %d\n", healthcheckPath, response.StatusCode)
		return 1
	}
	return 0
}
