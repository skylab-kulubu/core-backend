package skypass

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/googlewallet"
)

// The settings of SkyPass in Google Wallet (docs/skypass-google-wallet.md).
// Off unless SKYPASS_GOOGLE_WALLET_ENABLED is true; then every required
// setting must be right or core does not start. Errors name a variable,
// never a value.
const (
	GoogleWalletEnabledEnv        = "SKYPASS_GOOGLE_WALLET_ENABLED"
	GoogleWalletIssuerIDEnv       = "SKYPASS_GOOGLE_WALLET_ISSUER_ID"
	GoogleWalletClassSuffixEnv    = "SKYPASS_GOOGLE_WALLET_CLASS_SUFFIX"
	GoogleWalletServiceAccountEnv = "SKYPASS_GOOGLE_WALLET_SERVICE_ACCOUNT_JSON"
	GoogleWalletTOTPKeyEnv        = "SKYPASS_GOOGLE_WALLET_TOTP_KEY"
	GoogleWalletOriginsEnv        = "SKYPASS_GOOGLE_WALLET_ORIGINS"
	GoogleWalletLogoURLEnv        = "SKYPASS_GOOGLE_WALLET_LOGO_URL"
)

// GoogleWalletConfig is the issuer core writes passes as.
type GoogleWalletConfig struct {
	Enabled bool
	// IssuerID is the Google Wallet issuer's number.
	IssuerID string
	// ClassSuffix names this environment's class: one GenericClass per
	// environment (issuerID.suffix), so sandbox passes never share a class
	// with production ones.
	ClassSuffix string
	Account     googlewallet.ServiceAccount
	// TOTPKey derives every pass's TOTP secret (32 bytes).
	TOTPKey []byte
	// Origins are the web origins that may show Google's save button for
	// core's links (the JWT's origins claim). Optional.
	Origins []string
	// LogoURL is the pass's logo, an https image. Optional: Google then
	// shows the first letter of the card title.
	LogoURL string
}

func (c GoogleWalletConfig) ClassID() string {
	return c.IssuerID + "." + c.ClassSuffix
}

var (
	issuerIDPattern    = regexp.MustCompile(`^[0-9]{1,32}$`)
	classSuffixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// GoogleWalletConfigFromEnv reads the settings. Unset or false: off, and
// nothing else is read.
func GoogleWalletConfigFromEnv(getenv func(string) string) (GoogleWalletConfig, error) {
	switch strings.TrimSpace(getenv(GoogleWalletEnabledEnv)) {
	case "", "false":
		return GoogleWalletConfig{}, nil
	case "true":
	default:
		return GoogleWalletConfig{}, fmt.Errorf("%s must be true or false", GoogleWalletEnabledEnv)
	}
	config := GoogleWalletConfig{Enabled: true}
	var missing []string
	for _, name := range []string{GoogleWalletIssuerIDEnv, GoogleWalletClassSuffixEnv, GoogleWalletServiceAccountEnv, GoogleWalletTOTPKeyEnv} {
		if strings.TrimSpace(getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return GoogleWalletConfig{}, fmt.Errorf("%s required with %s=true", strings.Join(missing, ", "), GoogleWalletEnabledEnv)
	}
	config.IssuerID = strings.TrimSpace(getenv(GoogleWalletIssuerIDEnv))
	if !issuerIDPattern.MatchString(config.IssuerID) {
		return GoogleWalletConfig{}, fmt.Errorf("%s must be the issuer's number", GoogleWalletIssuerIDEnv)
	}
	config.ClassSuffix = strings.TrimSpace(getenv(GoogleWalletClassSuffixEnv))
	if !classSuffixPattern.MatchString(config.ClassSuffix) {
		return GoogleWalletConfig{}, fmt.Errorf("%s must be letters, digits, '.', '_' or '-' (at most 64)", GoogleWalletClassSuffixEnv)
	}
	account, err := googlewallet.ParseServiceAccount(getenv(GoogleWalletServiceAccountEnv))
	if err != nil {
		return GoogleWalletConfig{}, fmt.Errorf("%s: %w", GoogleWalletServiceAccountEnv, err)
	}
	config.Account = account
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(getenv(GoogleWalletTOTPKeyEnv)))
	if err != nil || len(key) != 32 {
		return GoogleWalletConfig{}, fmt.Errorf("%s must be an unpadded base64url-encoded 32-byte key", GoogleWalletTOTPKeyEnv)
	}
	config.TOTPKey = key
	for _, raw := range strings.Split(getenv(GoogleWalletOriginsEnv), ",") {
		origin := strings.TrimSpace(raw)
		if origin == "" {
			continue
		}
		if !validOrigin(origin) {
			return GoogleWalletConfig{}, fmt.Errorf("%s must list origins like https://hesap.yildizskylab.com", GoogleWalletOriginsEnv)
		}
		config.Origins = append(config.Origins, origin)
	}
	if logo := strings.TrimSpace(getenv(GoogleWalletLogoURLEnv)); logo != "" {
		u, err := url.Parse(logo)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return GoogleWalletConfig{}, fmt.Errorf("%s must be an https URL", GoogleWalletLogoURLEnv)
		}
		config.LogoURL = logo
	}
	return config, nil
}

func validOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"))
}
