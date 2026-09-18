package media

import (
	"fmt"
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
