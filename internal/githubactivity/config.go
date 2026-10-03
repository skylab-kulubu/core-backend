package githubactivity

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The settings. The private key comes from OpenBao through a Dokploy
// reference (ADR-0049); every error names a variable, never a value.
const (
	OrgEnv            = "GITHUB_ACTIVITY_ORG"
	AppIDEnv          = "GITHUB_ACTIVITY_APP_ID"
	InstallationIDEnv = "GITHUB_ACTIVITY_INSTALLATION_ID"
	PrivateKeyEnv     = "GITHUB_ACTIVITY_APP_PRIVATE_KEY"
	WindowDaysEnv     = "GITHUB_ACTIVITY_WINDOW_DAYS"
)

const (
	// DefaultWindowDays is the window when GITHUB_ACTIVITY_WINDOW_DAYS is unset.
	DefaultWindowDays = 30
	// MaxWindowDays bounds the window: twice it is how far back commits are
	// read.
	MaxWindowDays = 90
)

// Config is how core reaches the organisation's GitHub App.
type Config struct {
	Org   string
	AppID int64
	// InstallationID is the app's installation on Org. Zero asks GitHub for
	// it (GET /orgs/{org}/installation).
	InstallationID int64
	PrivateKey     *rsa.PrivateKey
	WindowDays     int
}

var orgLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// ConfigFromEnv reads the settings. ok is false while none of them is set:
// the feature is off and its route is not served. A setting that is set but
// wrong, or one missing beside the others, is an error.
func ConfigFromEnv(getenv func(string) string) (config Config, ok bool, err error) {
	org := strings.TrimSpace(getenv(OrgEnv))
	appID := strings.TrimSpace(getenv(AppIDEnv))
	installation := strings.TrimSpace(getenv(InstallationIDEnv))
	key := getenv(PrivateKeyEnv)
	days := strings.TrimSpace(getenv(WindowDaysEnv))
	if org == "" && appID == "" && installation == "" && strings.TrimSpace(key) == "" {
		return Config{}, false, nil
	}
	var problems []string
	if org == "" {
		problems = append(problems, OrgEnv+" is required")
	} else if !orgLogin.MatchString(org) {
		problems = append(problems, OrgEnv+" must be a GitHub organisation login")
	}
	config.Org = org
	if config.AppID, err = positiveID(appID); err != nil {
		problems = append(problems, AppIDEnv+" "+err.Error())
	}
	if installation != "" {
		if config.InstallationID, err = positiveID(installation); err != nil {
			problems = append(problems, InstallationIDEnv+" "+err.Error())
		}
	}
	if config.PrivateKey, err = ParsePrivateKey(key); err != nil {
		problems = append(problems, PrivateKeyEnv+" "+err.Error())
	}
	config.WindowDays = DefaultWindowDays
	if days != "" {
		n, convErr := strconv.Atoi(days)
		if convErr != nil || n < 1 || n > MaxWindowDays {
			problems = append(problems, fmt.Sprintf("%s must be a whole number of days from 1 to %d", WindowDaysEnv, MaxWindowDays))
		} else {
			config.WindowDays = n
		}
	}
	if len(problems) > 0 {
		return Config{}, true, errors.New(strings.Join(problems, "; "))
	}
	return config, true, nil
}

func positiveID(raw string) (int64, error) {
	if raw == "" {
		return 0, errors.New("is required")
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, errors.New("must be a positive number")
	}
	return n, nil
}

// ParsePrivateKey reads the GitHub App's RSA private key: the .pem file GitHub
// hands out (PKCS #1, or PKCS #8), as it is, with its line breaks written as
// "\n", or base64-encoded on one line, which is how the wizard stores it in
// OpenBao so the Dokploy environment stays one line per variable. Errors never
// quote the input.
func ParsePrivateKey(raw string) (*rsa.PrivateKey, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, errors.New("is required")
	}
	if !strings.Contains(text, "-----BEGIN") {
		decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(text), ""))
		if err != nil {
			return nil, errors.New("must be a PEM private key or one encoded in base64")
		}
		text = strings.TrimSpace(string(decoded))
	}
	if !strings.Contains(text, "\n") && strings.Contains(text, `\n`) {
		text = strings.ReplaceAll(text, `\n`, "\n")
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("must be a PEM private key or one encoded in base64")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("is not a readable RSA private key")
		}
		return key, nil
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("is not a readable private key")
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("must be an RSA key (GitHub App keys are)")
		}
		return key, nil
	default:
		return nil, errors.New("must be a private key PEM block")
	}
}
