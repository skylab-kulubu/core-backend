package skypass

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Service interface {
	BindCard(ctx context.Context, p authz.Principal, uid string, target *uuid.UUID) (user.User, error)
	Lookup(ctx context.Context, p authz.Principal, uid string) (Holder, error)
	Mint(ctx context.Context, p authz.Principal) (Token, error)
	Verify(ctx context.Context, p authz.Principal, token string) (Holder, error)
	// HolderFrom is whose credential the door scanned, for a check-in: an
	// in-app token, a Google Wallet code (spent by this call) or a bound
	// Student-card UID. The caller decides whether the scanner p may check
	// anyone in; p only keys the budget of wrong Wallet codes.
	HolderFrom(ctx context.Context, p authz.Principal, token, uid string) (Holder, error)
	JWKS() JWKS
	// WalletStatus tells the person's app whether to offer "Add to Google
	// Wallet" and whether they have a pass.
	WalletStatus(ctx context.Context, p authz.Principal) (WalletStatus, error)
	// GoogleWalletLink writes the caller's pass to Google and returns its
	// save link.
	GoogleWalletLink(ctx context.Context, p authz.Principal) (WalletLink, error)
	// RevokeGoogleWallet ends the caller's pass: its codes stop at once and
	// Google's copy goes inactive. The next link is a new pass.
	RevokeGoogleWallet(ctx context.Context, p authz.Principal) error
}

type service struct {
	users  user.Store
	authz  authz.Authorizer
	signer *Signer
	wallet *Wallet
}

func NewService(users user.Store, az authz.Authorizer, signer *Signer) Service {
	return &service{users: users, authz: az, signer: signer}
}

// NewServiceWithWallet is NewService that also reads Google Wallet codes and
// issues Wallet passes (docs/skypass-google-wallet.md).
func NewServiceWithWallet(users user.Store, az authz.Authorizer, signer *Signer, wallet *Wallet) Service {
	return &service{users: users, authz: az, signer: signer, wallet: wallet}
}

func (s *service) BindCard(ctx context.Context, p authz.Principal, uid string, target *uuid.UUID) (user.User, error) {
	if p.ID == "" {
		return user.User{}, ErrForbidden
	}
	normalized, err := NormalizeUID(uid)
	if err != nil {
		return user.User{}, err
	}
	actor, err := uuid.Parse(p.ID)
	if err != nil {
		return user.User{}, ErrInvalid
	}
	id := actor
	if target != nil && *target != uuid.Nil && *target != actor {
		if normalized != "" {
			return user.User{}, ErrForbidden
		}
		if !s.authz.Allow(p, authz.Resource{Type: authz.TypeUser}, authz.Update) {
			return user.User{}, ErrForbidden
		}
		id = *target
	}
	got, err := s.users.SetStudentCardUID(ctx, id, normalized)
	if errors.Is(err, user.ErrConflict) {
		return user.User{}, ErrConflict
	}
	if errors.Is(err, user.ErrNotFound) || errors.Is(err, user.ErrAccountBlocked) {
		return user.User{}, ErrNotFound
	}
	return got, err
}

func (s *service) Lookup(ctx context.Context, p authz.Principal, uid string) (Holder, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.Validate) {
		return Holder{}, ErrForbidden
	}
	normalized, err := NormalizeUID(uid)
	if err != nil {
		return Holder{}, err
	}
	if normalized == "" {
		return Holder{}, ErrInvalid
	}
	u, err := s.users.FindByStudentCardUID(ctx, normalized)
	if errors.Is(err, user.ErrNotFound) {
		return Holder{}, ErrNotFound
	}
	if err != nil {
		return Holder{}, err
	}
	return holder(u), nil
}

func (s *service) Mint(ctx context.Context, p authz.Principal) (Token, error) {
	u, err := s.caller(ctx, p)
	if err != nil {
		return Token{}, err
	}
	return s.signer.Mint(u)
}

// caller is the signed-in person's active row.
func (s *service) caller(ctx context.Context, p authz.Principal) (user.User, error) {
	if p.ID == "" {
		return user.User{}, ErrForbidden
	}
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return user.User{}, ErrInvalid
	}
	u, err := s.users.Get(ctx, id)
	return activeUser(u, err)
}

func (s *service) WalletStatus(ctx context.Context, p authz.Principal) (WalletStatus, error) {
	u, err := s.caller(ctx, p)
	if err != nil {
		return WalletStatus{}, err
	}
	return s.wallet.status(ctx, u.ID)
}

func (s *service) GoogleWalletLink(ctx context.Context, p authz.Principal) (WalletLink, error) {
	u, err := s.caller(ctx, p)
	if err != nil {
		return WalletLink{}, err
	}
	return s.wallet.googleLink(ctx, u)
}

func (s *service) RevokeGoogleWallet(ctx context.Context, p authz.Principal) error {
	u, err := s.caller(ctx, p)
	if err != nil {
		return err
	}
	return s.wallet.revokeGoogle(ctx, u.ID)
}

// walletHolder is the holder of a Google Wallet code.
func (s *service) walletHolder(ctx context.Context, p authz.Principal, token string, consume bool) (Holder, error) {
	u, err := s.wallet.holder(ctx, p.ID, token, consume)
	if err != nil {
		return Holder{}, err
	}
	return holder(u), nil
}

func (s *service) Verify(ctx context.Context, p authz.Principal, token string) (Holder, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.Validate) {
		return Holder{}, ErrForbidden
	}
	if IsWalletCode(token) {
		// A look at who this is: the code is not spent.
		return s.walletHolder(ctx, p, token, false)
	}
	claims, err := s.signer.Verify(token)
	if err != nil {
		return Holder{}, err
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Holder{}, ErrInvalid
	}
	u, err := s.users.Get(ctx, id)
	u, err = activeUser(u, err)
	if err != nil {
		return Holder{}, err
	}
	return holder(u), nil
}

func (s *service) HolderFrom(ctx context.Context, p authz.Principal, token, uid string) (Holder, error) {
	token = strings.TrimSpace(token)
	uid = strings.TrimSpace(uid)
	if token != "" && IsWalletCode(token) {
		return s.walletHolder(ctx, p, token, true)
	}
	if token != "" {
		claims, err := s.signer.Verify(token)
		if err != nil {
			return Holder{}, err
		}
		id, err := uuid.Parse(claims.Subject)
		if err != nil {
			return Holder{}, ErrInvalid
		}
		u, err := s.users.Get(ctx, id)
		u, err = activeUser(u, err)
		if err != nil {
			return Holder{}, err
		}
		return holder(u), nil
	}
	normalized, err := NormalizeUID(uid)
	if err != nil {
		return Holder{}, err
	}
	if normalized == "" {
		return Holder{}, ErrInvalid
	}
	u, err := s.users.FindByStudentCardUID(ctx, normalized)
	if errors.Is(err, user.ErrNotFound) {
		return Holder{}, ErrNotFound
	}
	if err != nil {
		return Holder{}, err
	}
	return holder(u), nil
}

func (s *service) JWKS() JWKS {
	return s.signer.JWKS()
}

func holder(u user.User) Holder {
	return Holder{
		ID:        u.ID,
		SkyNumber: u.SkyNumber,
		FirstName: u.FirstName,
		LastName:  u.LastName,
	}
}

func activeUser(u user.User, err error) (user.User, error) {
	if errors.Is(err, user.ErrNotFound) || errors.Is(err, user.ErrAccountBlocked) {
		return user.User{}, ErrNotFound
	}
	if err != nil {
		return user.User{}, err
	}
	if u.AccountState != "" && u.AccountState != user.AccountActive {
		return user.User{}, ErrNotFound
	}
	return u, nil
}
