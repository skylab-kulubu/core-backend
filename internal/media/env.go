package media

import (
	"fmt"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/mediaframe"
)

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// PublicBaseFromEnv is the configured public base: CDN_BASE, else
// R2_PUBLIC_URL; empty when neither is set (DefaultPublicBase then).
func PublicBaseFromEnv(getenv func(string) string) string {
	return firstNonEmpty(getenv("CDN_BASE"), getenv("R2_PUBLIC_URL"))
}

func BlobAndCDN(getenv func(string) string) (BlobStore, string, error) {
	endpoint := firstNonEmpty(getenv("R2_ENDPOINT"))
	bucket := firstNonEmpty(getenv("R2_BUCKET"), getenv("R2_BUCKET_NAME"))
	access := firstNonEmpty(getenv("R2_ACCESS_KEY"))
	secret := firstNonEmpty(getenv("R2_SECRET_KEY"))
	cdn := PublicBaseFromEnv(getenv)

	any := endpoint != "" || bucket != "" || access != "" || secret != ""
	if !any {
		return NewMemoryBlob(), cdn, nil
	}
	if endpoint == "" || bucket == "" || access == "" || secret == "" {
		return nil, "", fmt.Errorf("R2_ENDPOINT, R2_BUCKET or R2_BUCKET_NAME, R2_ACCESS_KEY, and R2_SECRET_KEY are all required together")
	}
	return NewR2(R2Config{
		Endpoint:  endpoint,
		AccessKey: access,
		SecretKey: secret,
		Bucket:    bucket,
	}), cdn, nil
}

// FrameAddrFromEnv is the frame service's address (MEDIA_FRAME_ADDR,
// host:port on the internal network): empty when it is unset, which turns
// video frames off. Anything but host:port stops core at startup.
func FrameAddrFromEnv(getenv func(string) string) (string, error) {
	raw := strings.TrimSpace(getenv(FrameAddrEnv))
	if raw == "" {
		return "", nil
	}
	addr, err := mediaframe.ParseAddr(raw)
	if err != nil {
		return "", fmt.Errorf("%s must be the frame service's host:port (such as media-frame:8080): %w", FrameAddrEnv, err)
	}
	return addr, nil
}
