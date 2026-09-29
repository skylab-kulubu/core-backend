package mediaframe

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// maxAllocBytes bounds one allocation of ffmpeg's (its -max_alloc): a
// decoded 4K frame is a few tens of MiB.
const maxAllocBytes = 256 << 20

// ffmpegArgs are ffmpeg's arguments for the frame of input at at, at most
// maxDimension pixels on its longer side:
//
//   - -nostdin, and quiet: only errors on its standard error;
//   - only the loopback proxy's http (-protocol_whitelist http,tcp): no
//     file, no other protocol, and never the network beyond the proxy;
//   - one decoding thread, and no allocation above maxAllocBytes;
//   - the input read as an MP4 alone (-f mov, the mov/mp4 demuxer), with its
//     external data references off (-enable_drefs 0): ffmpeg never guesses
//     the format from the content or a MIME type. -protocol_whitelist
//     limits protocols, not hosts: a playlist (HLS), a concat list or a
//     reference ffmpeg took the input for would open other http addresses
//     on the internal network, past the proxy;
//   - -ss before -i: a fast seek, to the keyframe before at, by the
//     video's index (a faststart MP4's moov is read first);
//   - the first video stream only, one frame, no audio, subtitles or data;
//   - scaled down (never up) to maxDimension, turned upright by the
//     video's rotation (ffmpeg's autorotate);
//   - one JPEG on its standard output (pipe:1): nothing on disk.
func ffmpegArgs(input string, at time.Duration, maxDimension int) []string {
	return []string{
		"-nostdin", "-hide_banner", "-nostats", "-loglevel", "error",
		"-max_alloc", fmt.Sprint(maxAllocBytes),
		"-protocol_whitelist", "http,tcp",
		"-threads", "1",
		"-f", "mov", "-enable_drefs", "0",
		"-ss", fmt.Sprintf("%d.%03d", at/time.Second, (at%time.Second)/time.Millisecond),
		"-i", input,
		"-map", "0:v:0", "-frames:v", "1", "-an", "-sn", "-dn",
		"-vf", fmt.Sprintf("scale=w='min(iw,%d)':h='min(ih,%d)':force_original_aspect_ratio=decrease", maxDimension, maxDimension),
		"-f", "image2pipe", "-c:v", "mjpeg", "-q:v", "2",
		"pipe:1",
	}
}

// FFmpeg is the Runner of the ffmpeg at path: no standard input, an empty
// environment (no proxy settings reach it), its own process group, killed
// whole when the request's time runs out.
func FFmpeg(path string) Runner {
	return RunnerFunc(func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		cmd.Env = []string{}
		cmd.WaitDelay = 2 * time.Second
		ownProcessGroup(cmd)
		return cmd.Run()
	})
}

// FFmpegVersion is the first line of `ffmpeg -version`: the service checks
// at startup that ffmpeg runs.
func FFmpegVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-version")
	cmd.Stdout = &out
	cmd.Env = []string{}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mediaframe: %s -version: %w", path, err)
	}
	line, _, _ := bytes.Cut(out.Bytes(), []byte("\n"))
	return string(bytes.TrimSpace(line)), nil
}
