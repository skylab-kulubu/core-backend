package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/doorqr"
)

func TestDoorQRGateSharesTheSkyPassKeyAcrossReplicas(t *testing.T) {
	t.Parallel()
	passKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{doorqr.ModeEnv: "qr"}
	getenv := func(k string) string { return env[k] }
	a, err := doorQRGate(getenv, passKey)
	if err != nil {
		t.Fatal(err)
	}
	b, err := doorQRGate(getenv, passKey)
	if err != nil {
		t.Fatal(err)
	}
	if a.Mode() != doorqr.ModeQR {
		t.Fatalf("mode %s", a.Mode())
	}
	session, ev := uuid.New(), uuid.New()
	pass, err := a.Mint(session, ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Admit(pass.Token, session, ev); err != nil {
		t.Fatalf("second replica: %v", err)
	}
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c, err := doorQRGate(getenv, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Admit(pass.Token, session, ev); err == nil {
		t.Fatal("another SkyPass key accepted the door QR")
	}
}

func TestDoorQRGateRefusesAModeTypo(t *testing.T) {
	t.Parallel()
	passKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doorQRGate(func(k string) string {
		if k == doorqr.ModeEnv {
			return "QR"
		}
		return ""
	}, passKey); err == nil {
		t.Fatal("accepted a mode typo")
	}
}
