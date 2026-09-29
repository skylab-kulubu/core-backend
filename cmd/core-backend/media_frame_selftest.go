package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"image/jpeg"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/mediaframe"
)

// mediaFrameSelfTestCommandName checks the frame service instead of running
// the server: `core-backend media-frame-selftest [-addr host:port]`. It
// stores a tiny sample video (mediaframe.SampleVideo: red for its first
// second, blue for its second) in core's public bucket under a temporary
// key (pending/selftest/…, which the CDN does not serve and R2's lifecycle
// rule clears), asks the frame service at MEDIA_FRAME_ADDR (or -addr) for
// its frame at one second by a presigned GET, and deletes the key. It
// exits 0 only when the frame is the sample's blue 320x180 one. The
// deploy check runs it inside core's container
// (ops/wizards/media-frame-wizard.sh in sky_lab_genel), so it also proves
// core reaches the service over the internal network, and the service
// reaches R2 with core's presigned address (its allowlist names R2's host).
const mediaFrameSelfTestCommandName = "media-frame-selftest"

// mediaFrameSelfTestTimeout bounds the whole check: the frame service's
// queue and ffmpeg's run, and the storage calls around them.
const mediaFrameSelfTestTimeout = 3 * time.Minute

// The sample's size.
const selfTestWidth, selfTestHeight = 320, 180

// runMediaFrameSelfTest exits 0 when the frame service answers the
// sample's frame at one second, 1 when it answers another or none or
// cannot be reached (or the sample cannot be stored), and 2 without a
// usable address or R2.
func runMediaFrameSelfTest(args []string, getenv func(string) string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet(mediaFrameSelfTestCommandName, flag.ContinueOnError)
	flags.SetOutput(errOut)
	addrFlag := flags.String("addr", "", "the frame service's host:port; defaults to "+media.FrameAddrEnv)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(errOut, "%s: %s\n", mediaFrameSelfTestCommandName, fmt.Sprintf(format, a...))
		return code
	}
	addrEnv := getenv
	if *addrFlag != "" {
		addrEnv = func(name string) string {
			if name == media.FrameAddrEnv {
				return *addrFlag
			}
			return ""
		}
	}
	addr, err := media.FrameAddrFromEnv(addrEnv)
	if err != nil {
		return fail(2, "%v", err)
	}
	if addr == "" {
		return fail(2, "set %s (or pass -addr host:port)", media.FrameAddrEnv)
	}
	blobs, _, err := media.BlobAndCDN(getenv)
	if err != nil {
		return fail(2, "%v", err)
	}
	r2, ok := blobs.(*media.R2)
	if !ok {
		return fail(2, "core has no R2 (R2_ENDPOINT, R2_BUCKET, R2_ACCESS_KEY, R2_SECRET_KEY): the frame service reads videos from it")
	}
	client, err := mediaframe.NewClient(addr)
	if err != nil {
		return fail(2, "%v", err)
	}

	sample, err := mediaframe.SampleVideo(selfTestWidth, selfTestHeight)
	if err != nil {
		return fail(1, "make the sample video: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), mediaFrameSelfTestTimeout)
	defer cancel()
	key := "pending/selftest/frame-" + uuid.NewString() + ".mp4"
	if err := r2.Put(ctx, key, sample, media.BlobMetadata{ContentType: "video/mp4"}); err != nil {
		return fail(1, "store the sample video in R2: %v", err)
	}
	defer func() {
		deleteCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r2.Delete(deleteCtx, key); err != nil {
			fmt.Fprintf(errOut, "%s: delete the sample video (%s): %v; R2's lifecycle rule on pending/ clears it\n", mediaFrameSelfTestCommandName, key, err)
		}
	}()
	address, err := r2.PresignGet(ctx, key, 2*time.Minute)
	if err != nil {
		return fail(1, "presign the sample video: %v", err)
	}
	frame, err := client.Frame(ctx, address, media.FrameAt)
	var problem *mediaframe.Problem
	switch {
	case errors.Is(err, mediaframe.ErrUnavailable):
		return fail(1, "the frame service at %s cannot be reached or is busy: %v", addr, err)
	case errors.As(err, &problem) && problem.Code == mediaframe.CodeURLNotAllowed:
		return fail(1, "the frame service at %s refuses R2's address: its MEDIA_FRAME_ALLOWED_HOSTS must name the host of core's R2_ENDPOINT (%v)", addr, err)
	case err != nil:
		return fail(1, "the frame service at %s: %v", addr, err)
	}
	img, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		return fail(1, "the frame service at %s answered no JPEG: %v", addr, err)
	}
	bounds := img.Bounds()
	r, g, b, _ := img.At(bounds.Min.X+bounds.Dx()/2, bounds.Min.Y+bounds.Dy()/2).RGBA()
	blue := b>>8 > 150 && r>>8+80 < b>>8 && g>>8+80 < b>>8
	if bounds.Dx() != selfTestWidth || bounds.Dy() != selfTestHeight || !blue {
		return fail(1, "the frame service at %s answered a %dx%d frame that is not the sample's at 1 s (blue, %dx%d)",
			addr, bounds.Dx(), bounds.Dy(), selfTestWidth, selfTestHeight)
	}
	fmt.Fprintf(out, "frame service at %s: a %dx%d JPEG of %d bytes, the sample's frame at 1 s\nFRAME OK\n",
		addr, bounds.Dx(), bounds.Dy(), len(frame))
	return 0
}
