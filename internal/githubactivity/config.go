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
	"time"
)

// The settings. The private key comes from OpenBao through a Dokploy
// reference (ADR-0049); every error names a variable, never a value.
const (
	OrgEnv            = "GITHUB_ACTIVITY_ORG"
	AppIDEnv          = "GITHUB_ACTIVITY_APP_ID"
	InstallationIDEnv = "GITHUB_ACTIVITY_INSTALLATION_ID"
	PrivateKeyEnv     = "GITHUB_ACTIVITY_APP_PRIVATE_KEY"
	WindowDaysEnv     = "GITHUB_ACTIVITY_WINDOW_DAYS"
	WorkersEnv        = "GITHUB_ACTIVITY_WORKERS"
	MaxPagesEnv       = "GITHUB_ACTIVITY_MAX_PAGES"
	RefreshTimeoutEnv = "GITHUB_ACTIVITY_REFRESH_TIMEOUT"
)

const (
	// DefaultWindowDays is the window when GITHUB_ACTIVITY_WINDOW_DAYS is unset.
	DefaultWindowDays = 30
	// MaxWindowDays bounds the window: twice it is how far back commits are
	// read.
	MaxWindowDays = 90
	// DefaultWorkers is how many repositories are read at once.
	DefaultWorkers = 4
	MaxWorkers     = 16
	// DefaultMaxPages is the pages (of 100) read of one repository's commits
	// or pull requests before it is marked truncated.
	DefaultMaxPages = 10
	MaxMaxPages     = 50
	// DefaultRefreshTimeout bounds one read of GitHub.
	DefaultRefreshTimeout = 45 * time.Second
	MinRefreshTimeout     = 10 * time.Second
	MaxRefreshTimeout     = 5 * time.Minute
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
	// The read's budget; zero values take the defaults.
	Workers        int
	MaxPages       int
	RefreshTimeout time.Duration
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
	if config.Workers, err = boundedInt(getenv(WorkersEnv), DefaultWorkers, 1, MaxWorkers); err != nil {
		problems = append(problems, WorkersEnv+" "+err.Error())
	}
	if config.MaxPages, err = boundedInt(getenv(MaxPagesEnv), DefaultMaxPages, 1, MaxMaxPages); err != nil {
		problems = append(problems, MaxPagesEnv+" "+err.Error())
	}
	config.RefreshTimeout = DefaultRefreshTimeout
	if raw := strings.TrimSpace(getenv(RefreshTimeoutEnv)); raw != "" {
		d, convErr := time.ParseDuration(raw)
		if convErr != nil || d < MinRefreshTimeout || d > MaxRefreshTimeout {
			problems = append(problems, fmt.Sprintf("%s must be a duration from %s to %s", RefreshTimeoutEnv, MinRefreshTimeout, MaxRefreshTimeout))
		} else {
			config.RefreshTimeout = d
		}
	}
	if len(problems) > 0 {
		return Config{}, true, errors.New(strings.Join(problems, "; "))
	}
	return config, true, nil
}

func boundedInt(raw string, fallback, low, high int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < low || n > high {
		return 0, fmt.Errorf("must be a whole number from %d to %d", low, high)
	}
	return n, nil
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
