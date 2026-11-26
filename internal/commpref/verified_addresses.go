package commpref

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// The verified-address directory read for another question than the
// recipient one: whose is a guest's address (docs/guest-account-links.md).
// The same addresses match (ADR-0044: the Primary e-mail while verified,
// the YTÜ-linked School e-mail, the proven Personal e-mail) and the same
// reads make the directory, so a guest Ticket is never given to an account
// the recipient question would not give the address to.

// VerifiedAddress is one verified address of the directory.
type VerifiedAddress struct {
	// Holders are the accounts that hold it verified; more than one is
	// ambiguous: the address is nobody's.
	Holders []uuid.UUID
	// Kind is how the first holder holds it.
	Kind AddressKind
	// Enabled is Keycloak's enabled flag of the first holder.
	Enabled bool
}

// Owner is the one enabled account the address is: false when it is held by
// none, by more than one, or by a disabled account.
func (v VerifiedAddress) Owner() (uuid.UUID, bool) {
	if len(v.Holders) != 1 || !v.Enabled {
		return uuid.Nil, false
	}
	return v.Holders[0], true
}

func (x *addressIndex) verified(address string) (VerifiedAddress, bool) {
	holders := x.byAddress[address]
	if len(holders) == 0 {
		return VerifiedAddress{}, false
	}
	return VerifiedAddress{Holders: holders, Kind: x.kinds[address], Enabled: x.enabled[holders[0]]}, true
}

func (x *addressIndex) all() map[string]VerifiedAddress {
	out := make(map[string]VerifiedAddress, len(x.byAddress))
	for address := range x.byAddress {
		out[address], _ = x.verified(address)
	}
	return out
}

// errNoDirectory is a Recipients without Keycloak's reads.
var errNoDirectory = errors.New("communication preference: no address directory")

// VerifiedAddressOf is the directory's entry for address (trimmed, lower
// case), from the directory as the recipient question reads it: up to
// AddressDirectoryFresh old, AddressDirectoryMaxAge while Keycloak does not
// answer. false when no account holds it verified.
func (r *Recipients) VerifiedAddressOf(ctx context.Context, address string) (VerifiedAddress, bool, error) {
	if r == nil || r.dir == nil {
		return VerifiedAddress{}, false, errNoDirectory
	}
	index, err := r.index(ctx)
	if err != nil {
		return VerifiedAddress{}, false, err
	}
	v, ok := index.verified(normalizeAddress(address))
	return v, ok, nil
}

// FreshVerifiedAddresses reads the directory anew (or waits for the read
// already running), keeps it for the recipient question as any read, and
// answers every verified address of it. A read that failed less than 30
// seconds ago fails it at once.
func (r *Recipients) FreshVerifiedAddresses(ctx context.Context) (map[string]VerifiedAddress, error) {
	if r == nil || r.dir == nil {
		return nil, errNoDirectory
	}
	read := r.startRead(r.now())
	if read == nil {
		return nil, ErrAddressDirectoryUnavailable
	}
	select {
	case <-read.done:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", ErrAddressDirectoryUnavailable, ctx.Err())
	}
	if read.err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAddressDirectoryUnavailable, read.err)
	}
	r.mu.Lock()
	index := r.current
	r.mu.Unlock()
	return index.all(), nil
}

// ReadVerifiedAddresses reads the whole directory once from dir, the School
// e-mail's link from the identity provider ytuAlias, without keeping it (a
// one-off command).
func ReadVerifiedAddresses(ctx context.Context, dir AddressDirectory, ytuAlias string) (map[string]VerifiedAddress, error) {
	index, _, err := readAddressIndex(ctx, dir, ytuAlias, AddressDirectoryPage)
	if err != nil {
		return nil, err
	}
	return index.all(), nil
}
