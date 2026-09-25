package media

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func BlobAndCDN(getenv func(string) string) (BlobStore, string, error) {
	endpoint := firstNonEmpty(getenv("R2_ENDPOINT"))
	bucket := firstNonEmpty(getenv("R2_BUCKET"), getenv("R2_BUCKET_NAME"))
	access := firstNonEmpty(getenv("R2_ACCESS_KEY"))
	secret := firstNonEmpty(getenv("R2_SECRET_KEY"))
	cdn := firstNonEmpty(getenv("CDN_BASE"), getenv("R2_PUBLIC_URL"))

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

// CheckPrivateMediaFlag reads MEDIA_PRIVATE_ENABLED (default false). While it
// is off, private purposes are refused with ErrPrivateMediaDisabled. This
// build has no private Media storage (encryption and the private bucket), so
// turning the flag on is a configuration error: core refuses to start rather
// than let a private purpose near public storage.
func CheckPrivateMediaFlag(getenv func(string) string) error {
	raw := strings.TrimSpace(getenv("MEDIA_PRIVATE_ENABLED"))
	if raw == "" {
		return nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return errors.New("MEDIA_PRIVATE_ENABLED must be true or false")
	}
	if enabled {
		return errors.New("MEDIA_PRIVATE_ENABLED=true needs private Media storage, which this core does not have yet")
	}
	return nil
}
