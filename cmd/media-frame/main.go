// Command media-frame is the video frame service (media redesign ticket 25):
// it answers POST /frame with one JPEG frame of a video read from a
// presigned storage address, by ffmpeg, and GET /health. It runs in its own
// container (Dockerfile.frame), on the internal network only; core reaches
// it at MEDIA_FRAME_ADDR. See internal/mediaframe and "Video frames" in
// docs/media-lifecycle.md.
//
// Environment:
//
//	MEDIA_FRAME_ALLOWED_HOSTS  the storage hosts videos are read from (core's
//	                           R2_ENDPOINT, or its host), comma-separated;
//	                           required
//	PORT                       default 8080
//	MEDIA_FRAME_FFMPEG         ffmpeg's path; default ffmpeg
//
// `media-frame health` checks the running service (the container's health
// check).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/mediaframe"
)

// Environment names.
const (
	allowedHostsEnv = "MEDIA_FRAME_ALLOWED_HOSTS"
	portEnv         = "PORT"
	ffmpegEnv       = "MEDIA_FRAME_FFMPEG"
)

type config struct {
	addr    string
	ffmpeg  string
	allowed []string
}

func configFromEnv(getenv func(string) string) (config, error) {
	allowed, err := mediaframe.ParseAllowedHosts(getenv(allowedHostsEnv))
	if err != nil {
		return config{}, fmt.Errorf("%s: %w", allowedHostsEnv, err)
	}
	port := strings.TrimSpace(getenv(portEnv))
	if port == "" {
		port = "8080"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return config{}, fmt.Errorf("%s must be a port from 1 to 65535", portEnv)
	}
	ffmpeg := strings.TrimSpace(getenv(ffmpegEnv))
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	return config{addr: ":" + port, ffmpeg: ffmpeg, allowed: allowed}, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "health" {
		os.Exit(health(os.Getenv, os.Stderr))
	}
	if err := serve(os.Getenv); err != nil {
		log.Fatal(err)
	}
}

func serve(getenv func(string) string) error {
	cfg, err := configFromEnv(getenv)
	if err != nil {
		return err
	}
	version, err := mediaframe.FFmpegVersion(context.Background(), cfg.ffmpeg)
	if err != nil {
		return err
	}
	service, err := mediaframe.NewServer(mediaframe.Config{
		AllowedHosts: cfg.allowed,
		FFmpeg:       mediaframe.FFmpeg(cfg.ffmpeg),
		Logf:         log.Printf,
	})
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           service,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// A request waits for ffmpeg, then runs it (twice for a short clip)
		// within the timeout.
		WriteTimeout:   mediaframe.DefaultQueueWait + mediaframe.DefaultTimeout + 30*time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
	log.Printf("media-frame: listening on %s; %s; videos read only from %s", cfg.addr, version, strings.Join(cfg.allowed, ", "))
	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	failed := make(chan error, 1)
	go func() { failed <- server.ListenAndServe() }()
	select {
	case err := <-failed:
		return err
	case <-stop.Done():
	}
	ctx, done := context.WithTimeout(context.Background(), server.WriteTimeout)
	defer done()
	if err := server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// health exits 0 when the service on PORT answers GET /health with 200.
func health(getenv func(string) string, errOut io.Writer) int {
	port := strings.TrimSpace(getenv(portEnv))
	if port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		fmt.Fprintf(errOut, "media-frame health: %v\n", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(errOut, "media-frame health: %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
