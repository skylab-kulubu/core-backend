package skypass

import (
	"bytes"
	"testing"
	"time"
)

// RFC 6238 Appendix B, SHA-1 rows: the shared secret is the ASCII
// "12345678901234567890" (the Base16 key Google's rotating barcode example
// uses), 30-second steps from the Unix epoch, eight digits.
func TestTOTPMatchesRFC6238SHA1Vectors(t *testing.T) {
	t.Parallel()
	secret := []byte("12345678901234567890")
	for _, row := range []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	} {
		counter := totpCounter(time.Unix(row.unix, 0), 30*time.Second)
		if got := hotp(secret, counter, 8); got != row.want {
			t.Fatalf("T=%d: got %s, want %s", row.unix, got, row.want)
		}
	}
}

func TestWalletTOTPStepIsSixtySeconds(t *testing.T) {
	t.Parallel()
	start := time.Unix(1_800_000_000-1_800_000_000%60, 0)
	if totpCounter(start, WalletPeriod) != totpCounter(start.Add(59*time.Second), WalletPeriod) {
		t.Fatal("one code should last the whole minute")
	}
	if totpCounter(start, WalletPeriod)+1 != totpCounter(start.Add(60*time.Second), WalletPeriod) {
		t.Fatal("the code should change every 60 seconds")
	}
	if totpCounter(time.Unix(-5, 0), WalletPeriod) != 0 {
		t.Fatal("a time before the epoch has no step")
	}
}

func TestWalletSecretIsPerPassAndKeyed(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{7}, 32)
	other := bytes.Repeat([]byte{8}, 32)
	a := walletSecret(key, "AAAAAAAAAAAAAAAAAAAAAAAAAA")
	if len(a) != walletSecretBytes {
		t.Fatalf("secret is %d bytes", len(a))
	}
	if !bytes.Equal(a, walletSecret(key, "AAAAAAAAAAAAAAAAAAAAAAAAAA")) {
		t.Fatal("the same pass must get the same secret on every replica")
	}
	if bytes.Equal(a, walletSecret(key, "BBBBBBBBBBBBBBBBBBBBBBBBBB")) {
		t.Fatal("two passes share a secret")
	}
	if bytes.Equal(a, walletSecret(other, "AAAAAAAAAAAAAAAAAAAAAAAAAA")) {
		t.Fatal("the secret does not depend on the key")
	}
}

func TestWalletCodeParsing(t *testing.T) {
	t.Parallel()
	good := "SPW1:ABCDEFGHIJKLMNOPQRSTUVWXYZ:01234567"
	passID, code, ok := parseWalletCode("  " + good + "\n")
	if !ok || passID != "ABCDEFGHIJKLMNOPQRSTUVWXYZ" || code != "01234567" {
		t.Fatalf("parse %q %q %v", passID, code, ok)
	}
	if !IsWalletCode(good) || IsWalletCode("eyJhbGciOiJFUzI1NiJ9.e30.sig") || IsWalletCode("SKYPASS:1:Ada") {
		t.Fatal("IsWalletCode")
	}
	for _, bad := range []string{
		"",
		"SPW1:",
		"SPW1:ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		"SPW1:ABCDEFGHIJKLMNOPQRSTUVWXYZ:",
		"SPW1:ABCDEFGHIJKLMNOPQRSTUVWXY:01234567",    // short id
		"SPW1:abcdefghijklmnopqrstuvwxyz:01234567",   // not base32
		"SPW1:ABCDEFGHIJKLMNOPQRSTUVWXY1:01234567",   // 1 is not base32
		"SPW1:ABCDEFGHIJKLMNOPQRSTUVWXYZ:0123456",    // seven digits
		"SPW1:ABCDEFGHIJKLMNOPQRSTUVWXYZ:012345678",  // nine digits
		"SPW1:ABCDEFGHIJKLMNOPQRSTUVWXYZ:0123456a",   // not a digit
		"SPW2:ABCDEFGHIJKLMNOPQRSTUVWXYZ:01234567",   // other version
		"SPW1:ABCDEFGHIJKLMNOPQRSTUVWXYZ:01234567:1", // extra part
	} {
		if _, _, ok := parseWalletCode(bad); ok {
			t.Fatalf("parsed %q", bad)
		}
	}
}

func TestWalletPassIDsAreRandomBase32(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 64 {
		id, err := newWalletPassID()
		if err != nil {
			t.Fatal(err)
		}
		if !validWalletPassID(id) {
			t.Fatalf("pass id %q", id)
		}
		if seen[id] {
			t.Fatalf("pass id %q twice", id)
		}
		seen[id] = true
	}
}
