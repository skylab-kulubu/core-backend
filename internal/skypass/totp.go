package skypass

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- RFC 6238 TOTP with SHA-1 is the only algorithm Google Wallet's rotating barcode offers (TOTP_SHA1); HMAC-SHA-1 is not affected by SHA-1 collisions.
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"strings"
	"time"
)

// The Google Wallet SkyPass code (docs/skypass-google-wallet.md): the pass's
// rotating barcode shows SPW1:<pass id>:<TOTP>, which Wallet redraws on the
// device every WalletPeriod (ADR-0023: 60 seconds, not the 3-second example
// of Google's how-to). The pass id is public: it names the pass and seeds
// its secret. The secret never leaves core and Google.
const (
	WalletCodePrefix = "SPW1:"
	// WalletPeriod is the TOTP step (periodMillis 60000).
	WalletPeriod = 60 * time.Second
	// WalletDigits is the TOTP length (valueLength).
	WalletDigits = 8
	// walletSkewSteps is how many steps before and after now a code is
	// taken: one, for a phone clock or a scan that is up to a minute off.
	walletSkewSteps = 1

	walletPassIDLength = 26 // 16 random bytes, Base32 without padding
	walletSecretBytes  = 20 // RFC 4226 §4 recommends 160 bits
	walletSecretInfo   = "skylab skypass google wallet totp v1\x00"
)

var walletPassIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// IsWalletCode reports whether a scanned value is a Wallet code rather than
// an in-app SkyPass token.
func IsWalletCode(raw string) bool {
	return strings.HasPrefix(strings.TrimSpace(raw), WalletCodePrefix)
}

// parseWalletCode splits SPW1:<pass id>:<digits>.
func parseWalletCode(raw string) (passID, code string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(raw), WalletCodePrefix)
	if !found {
		return "", "", false
	}
	passID, code, found = strings.Cut(rest, ":")
	if !found || !validWalletPassID(passID) || len(code) != WalletDigits {
		return "", "", false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return "", "", false
		}
	}
	return passID, code, true
}

func validWalletPassID(id string) bool {
	if len(id) != walletPassIDLength {
		return false
	}
	for _, r := range id {
		if !(r >= 'A' && r <= 'Z') && !(r >= '2' && r <= '7') {
			return false
		}
	}
	return true
}

func newWalletPassID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return walletPassIDEncoding.EncodeToString(raw[:]), nil
}

// walletSecret is the pass's TOTP secret: HKDF-SHA256 of the Wallet key with
// the pass id. Nothing secret is stored per pass; without the key the pass
// ids (which every barcode shows) give nothing, and a new pass id is a new
// secret.
func walletSecret(key []byte, passID string) []byte {
	secret, err := hkdf.Key(sha256.New, key, nil, walletSecretInfo+passID, walletSecretBytes)
	if err != nil {
		// Only a length beyond 255 hash blocks fails.
		panic(err)
	}
	return secret
}

// totpCounter is the RFC 6238 step of t (T0 = the Unix epoch).
func totpCounter(t time.Time, period time.Duration) uint64 {
	seconds := t.Unix()
	if seconds < 0 {
		return 0
	}
	return uint64(seconds) / uint64(period/time.Second)
}

// hotp is the RFC 4226 value of counter: HMAC-SHA-1, dynamic truncation,
// digits decimal digits with leading zeros.
func hotp(secret []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	out := []byte(strings.Repeat("0", digits))
	value %= mod
	for i := digits - 1; i >= 0; i-- {
		out[i] = byte('0' + value%10)
		value /= 10
	}
	return string(out)
}
