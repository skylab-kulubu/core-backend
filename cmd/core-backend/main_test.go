package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestLoadSkyPassKeyPrefersExplicitP256Environment(t *testing.T) {
	want, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(want)
	if err != nil {
		t.Fatal(err)
	}
	raw := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	t.Setenv("SKYPASS_EC_PRIVATE_KEY", string(raw))
	t.Setenv("SKYPASS_RSA_PRIVATE_KEY", "invalid legacy value")

	got, err := loadSkyPassKey()
	if err != nil {
		t.Fatal(err)
	}
	if got.Curve != elliptic.P256() || got.D.Cmp(want.D) != 0 {
		t.Fatal("loaded key does not match explicit P-256 key")
	}
}

func TestLoadSkyPassKeyDerivesES256FromLegacyRSAEnvironment(t *testing.T) {
	legacy, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	raw := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(legacy)})
	t.Setenv("SKYPASS_EC_PRIVATE_KEY", "")
	t.Setenv("SKYPASS_RSA_PRIVATE_KEY", string(raw))

	key, err := loadSkyPassKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.Curve != elliptic.P256() {
		t.Fatalf("curve = %v", key.Curve)
	}
}

func TestGotenbergURLDefaultsToComposeService(t *testing.T) {
	t.Setenv("GOTENBERG_URL", "")
	if got := gotenbergURL(); got != "http://gotenberg:3000" {
		t.Fatalf("gotenberg URL = %q", got)
	}

	t.Setenv("GOTENBERG_URL", " http://pdf.internal:3010/ ")
	if got := gotenbergURL(); got != "http://pdf.internal:3010/" {
		t.Fatalf("explicit gotenberg URL = %q", got)
	}
}
