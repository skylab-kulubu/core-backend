package skypass

import (
	"context"
	"time"
)

type scannedAtKey struct{}

// WithScannedAt is ctx carrying the moment an offline door took a scan it
// sends later (docs/offline-door.md): a SkyPass QR's expiry is read at it.
// The route checks its bounds first. A Google Wallet code is a TOTP spent
// online and is not read at it.
func WithScannedAt(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, scannedAtKey{}, at)
}

// verify is the SkyPass QR's claims, its expiry read at the scan's moment
// when ctx carries one, else now.
func (s *service) verify(ctx context.Context, token string) (passClaims, error) {
	if at, ok := ctx.Value(scannedAtKey{}).(time.Time); ok {
		return s.signer.VerifyAt(token, at)
	}
	return s.signer.Verify(token)
}
