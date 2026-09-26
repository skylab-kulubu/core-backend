package media

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/transit"
)

// PrivateEnabledEnv is the private Media flag. Off, private purposes are
// refused and core needs none of the settings below.
const PrivateEnabledEnv = "MEDIA_PRIVATE_ENABLED"

// LinkSigningKeyEnv holds the key read links are signed with: 32 random
// bytes, unpadded base64url, the format of ACCOUNT_DELETION_RECEIPT_KEY.
const LinkSigningKeyEnv = "MEDIA_LINK_SIGNING_KEY"

// PrivateConfig is how core stores private Media: the Transit key that wraps
// each data key, the private bucket, and the read link key.
type PrivateConfig struct {
	Enabled bool
	Transit transit.Config
	Bucket  R2Config
	LinkKey []byte
	// LinkOrigin is core's public address, where read links point:
	// PUBLIC_API_ORIGIN. It has no default here, unlike certificate links: a
	// sandbox without it would hand out links to production.
	LinkOrigin string
}

var (
	transitMountPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
	transitKeyPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// PrivateConfigFromEnv reads the private Media settings. With the flag off it
// reads nothing else. With it on, every setting is required and checked, so
// that a mistake stops core at startup instead of failing the first Answer
// file. Errors name the variables, never their values.
func PrivateConfigFromEnv(getenv func(string) string) (PrivateConfig, error) {
	switch strings.TrimSpace(getenv(PrivateEnabledEnv)) {
	case "", "false":
		return PrivateConfig{}, nil
	case "true":
	default:
		return PrivateConfig{}, fmt.Errorf("%s must be true or false", PrivateEnabledEnv)
	}
	value := func(name string) string { return strings.TrimSpace(getenv(name)) }
	required := []string{
		"MEDIA_OPENBAO_ADDR", "MEDIA_TRANSIT_MOUNT", "MEDIA_TRANSIT_KEY", "MEDIA_OPENBAO_ROLE_ID", "MEDIA_OPENBAO_SECRET_ID",
		"R2_ENDPOINT", "R2_PRIVATE_BUCKET", "R2_PRIVATE_ACCESS_KEY", "R2_PRIVATE_SECRET_KEY", LinkSigningKeyEnv,
		"PUBLIC_API_ORIGIN",
	}
	var missing []string
	for _, name := range required {
		if value(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return PrivateConfig{}, fmt.Errorf("%s=true needs %s", PrivateEnabledEnv, strings.Join(missing, ", "))
	}

	var problems []error
	addr, err := url.Parse(value("MEDIA_OPENBAO_ADDR"))
	if err != nil || (addr.Scheme != "http" && addr.Scheme != "https") || addr.Host == "" || strings.Trim(addr.Path, "/") != "" {
		problems = append(problems, errors.New("MEDIA_OPENBAO_ADDR must be an http(s) address with no path, such as http://openbao:8200"))
	}
	mount := strings.Trim(value("MEDIA_TRANSIT_MOUNT"), "/")
	if !transitMountPattern.MatchString(mount) {
		problems = append(problems, errors.New("MEDIA_TRANSIT_MOUNT must be a mount path such as transit/sandbox"))
	}
	if !transitKeyPattern.MatchString(value("MEDIA_TRANSIT_KEY")) {
		problems = append(problems, errors.New("MEDIA_TRANSIT_KEY must be a Transit key name such as media"))
	}
	publicBucket := firstNonEmpty(getenv("R2_BUCKET"), getenv("R2_BUCKET_NAME"))
	if value("R2_PRIVATE_BUCKET") == publicBucket {
		problems = append(problems, errors.New("R2_PRIVATE_BUCKET must not be the public bucket (R2_BUCKET)"))
	}
	if value("R2_PRIVATE_ACCESS_KEY") == value("R2_ACCESS_KEY") {
		// The private bucket's token is scoped to it alone.
		problems = append(problems, errors.New("R2_PRIVATE_ACCESS_KEY must not be the public bucket's R2_ACCESS_KEY"))
	}
	origin, err := url.Parse(value("PUBLIC_API_ORIGIN"))
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" {
		problems = append(problems, errors.New("PUBLIC_API_ORIGIN must be core's public http(s) address"))
	}
	linkKey, err := base64.RawURLEncoding.DecodeString(value(LinkSigningKeyEnv))
	if err != nil || len(linkKey) != 32 {
		problems = append(problems, fmt.Errorf("%s must be an unpadded base64url-encoded 32-byte key", LinkSigningKeyEnv))
	}
	if len(problems) > 0 {
		return PrivateConfig{}, errors.Join(problems...)
	}
	return PrivateConfig{
		Enabled: true,
		Transit: transit.Config{
			Addr: value("MEDIA_OPENBAO_ADDR"), Mount: mount, Key: value("MEDIA_TRANSIT_KEY"),
			RoleID: value("MEDIA_OPENBAO_ROLE_ID"), SecretID: value("MEDIA_OPENBAO_SECRET_ID"),
		},
		Bucket: R2Config{
			Endpoint: value("R2_ENDPOINT"), Bucket: value("R2_PRIVATE_BUCKET"),
			AccessKey: value("R2_PRIVATE_ACCESS_KEY"), SecretKey: value("R2_PRIVATE_SECRET_KEY"),
		},
		LinkKey:    linkKey,
		LinkOrigin: strings.TrimRight(value("PUBLIC_API_ORIGIN"), "/"),
	}, nil
}
