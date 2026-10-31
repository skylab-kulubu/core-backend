package httpx

import (
	"context"
	"log"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
	"github.com/skylab-kulubu/core-backend/internal/competition"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/consent"
	"github.com/skylab-kulubu/core-backend/internal/dashboard"
	"github.com/skylab-kulubu/core-backend/internal/editablesites"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventmail"
	"github.com/skylab-kulubu/core-backend/internal/handlers"
	"github.com/skylab-kulubu/core-backend/internal/health"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/membershipterm"
	"github.com/skylab-kulubu/core-backend/internal/middlewares"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/skypass"
	"github.com/skylab-kulubu/core-backend/internal/subjectrequest"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// UserSync is the user sync (docs/user-sync.md): JIT wakes it when it opens a
// row, and /v1/metrics carries its counters.
type UserSync interface {
	Wake()
	Prometheus() string
}

type Deps struct {
	Users        user.Service
	Identity     identity.Service
	Events       event.Service
	Seasons      season.Service
	Tickets      ticket.Service
	Competitors  competitor.Service
	Competitions competition.Service
	// FeedbackInvites reads the Events' feedback invite counts; nil leaves
	// GET /v1/events/{id}/feedback out.
	FeedbackInvites handlers.FeedbackCounter
	// FeedbackSender sends a feedback form at once ("Şimdi gönder"); nil
	// leaves its two routes out.
	FeedbackSender handlers.FeedbackSender
	// FeedbackReminders cancels the reminders of a feedback form the panel
	// closed; nil leaves its two routes out.
	FeedbackReminders      handlers.FeedbackReminders
	Media                  media.Service
	URLs                   shorturl.Service
	Certificates           certificate.Service
	SkyPass                skypass.Service
	UserSync               UserSync
	EventMail              eventmail.Service
	ParseToken             func(string) (authn.Identity, error)
	URLAttributionGuard    handlers.URLAttributionGuard
	AccountAccessGate      accessgate.Reader
	AccountAccessMetrics   *accessgate.Metrics
	SelfDeletion           handlers.AccountDeletionService
	ParseSelfDeleteContext func(string, string) (authn.Identity, error)
	// ParseSelfDeleteSudo verifies the self-delete bearer with a sky-account
	// Sudo mode token (`X-Sky-Sudo`) instead of a fresh ID token. Nil refuses
	// every sudo proof.
	ParseSelfDeleteSudo func(context.Context, string, string) (authn.Identity, error)
	// ParseSelfDeleteBearer verifies the self-delete bearer alone, with the
	// sudo path's local rules, so a retry of an idempotency key Core already
	// accepted is answered after the deletion closed the session its sudo
	// proof is bound to. Nil (or a nil ParseSelfDeleteSudo) turns that off.
	ParseSelfDeleteBearer func(string) (authn.Identity, error)

	// TrustedProxies are the peers allowed to speak for a client through
	// `X-Forwarded-For`. An empty value falls back to clientip.Default().
	TrustedProxies clientip.Ranges

	// AccountErasureMetrics are the account erasure watchdog's gauges. Nil
	// while the erasure worker is off.
	AccountErasureMetrics interface{ Prometheus() string }

	// RetentionMetrics are the periodic destruction run's metrics
	// (docs/retention-sweep.md). Nil while RETENTION_SWEEP_MODE is off.
	RetentionMetrics interface{ Prometheus() string }

	// EventsMetrics are the Integration events consumer's state and
	// counters and its records' cleanup (docs/integration-events.md),
	// served on /v1/metrics. Nil leaves them out.
	EventsMetrics interface{ Prometheus() string }

	// MediaCDNPurgeMetrics are the media CDN purge's counters. Nil while
	// the purge is off.
	MediaCDNPurgeMetrics interface{ Prometheus() string }

	// TeamMailMetrics are the team membership mail's counters
	// (docs/team-membership-mail.md). Nil publishes none.
	TeamMailMetrics interface{ Prometheus() string }

	// ManagementTerms keeps the management membership terms
	// (docs/management-membership-terms.md): its metrics on /v1/metrics,
	// the capabilities' expiredManagementMemberships and, in apply mode,
	// the /v1/management-memberships routes. Nil serves none of them.
	ManagementTerms *membershipterm.Service

	// TeamDelegationMetrics count the client roles nobody has classified
	// that team delegation's protection reads found
	// (docs/team-delegation.md). Nil publishes none.
	TeamDelegationMetrics interface{ Prometheus() string }

	// MediaUploadLimiter is each person's single-step upload budget. Nil
	// uses media.DefaultUploadLimits.
	MediaUploadLimiter *media.UploadLimiter

	// ServiceClients are the products' service clients: a service
	// account's token of one of them speaks for its product. Nil configures
	// none.
	ServiceClients authz.ServiceClients

	// MediaLookupsPerMinute is each product's address lookup budget. Zero
	// uses handlers.DefaultMediaLookupsPerMinute.
	MediaLookupsPerMinute int

	// GuestApplyMetrics counts Guest apply requests by caller class and
	// outcome, and serves them on /v1/metrics. Nil counts into nothing.
	GuestApplyMetrics *handlers.GuestApplyMetrics

	// GuestApplyPublicIPLimit is what Guest apply's per-address budget does
	// when it runs out (GUEST_APPLY_PUBLIC_IP_LIMIT_MODE). Empty enforces.
	GuestApplyPublicIPLimit handlers.GuestApplyLimitMode

	// SkyPassWalletMetrics are SkyPass in Google Wallet's counters
	// (docs/skypass-google-wallet.md), served on /v1/metrics. Nil leaves
	// them out.
	SkyPassWalletMetrics interface{ Prometheus() string }

	// GuestCheckInMetrics counts guest self check-ins by door QR presence
	// and outcome, and serves them on /v1/metrics
	// (docs/guest-self-check-in.md). Nil leaves them out.
	GuestCheckInMetrics interface{ Prometheus() string }

	// DoorQRLimits are the door QR routes' budgets. Nil uses
	// handlers.DefaultDoorQRLimits.
	DoorQRLimits *handlers.DoorQRLimits

	// GroupOverage reads the Groups of a person whose token carries the
	// Group overage marker instead of the groups claim (ADR-0059), and
	// serves its counters on /v1/metrics. Nil refuses every marked token
	// with 503; tokens with their groups claim do not use it.
	GroupOverage *identity.OverageGroups

	// Dashboard answers the admin panel's summary
	// (docs/dashboard-summary.md). Nil leaves the route out.
	Dashboard dashboard.Service

	// Authz is the authorizer every service decides with; the capabilities
	// route answers from it (docs/authz-roles.md). Nil leaves
	// /v1/users/me/capabilities unserved: 404.
	Authz authz.Authorizer

	// EditableSites answers the capabilities route's editableSites: the
	// configured Site clients the caller holds cms:access on, a hint for
	// the UI (docs/authz-roles.md). Nil answers an empty list.
	EditableSites *editablesites.Sites

	// ManagedTeams answers the capabilities route's managedTeams: the Teams
	// the caller is a Leader of, read from Keycloak
	// (docs/team-delegation.md). Nil answers an empty list.
	ManagedTeams *identity.TeamDelegation

	// AppAssignments mounts the app assignment routes (APP_ASSIGNMENTS=on,
	// docs/app-assignments.md). Off they are not served: 404.
	AppAssignments bool

	// AuthzRoleMetrics are the role mode and its disagreements, served on
	// /v1/metrics. Nil serves nothing.
	AuthzRoleMetrics *authz.RoleMetrics

	// GithubActivity is the club's GitHub activity for the admin dashboard
	// (docs/github-activity.md). Nil (its settings unset) leaves
	// /v1/dashboard/github-activity unserved: 404.
	GithubActivity handlers.GithubActivitySource

	// SubjectRequests is the register of KVKK md. 11 data subject requests
	// (docs/subject-requests.md). Nil leaves /v1/subject-requests unserved.
	SubjectRequests subjectrequest.Service

	// Consents keeps contact consents (docs/contact-consents.md). Nil or
	// off (CONTACT_CONSENT_KEY unset) answers its routes 503 and records
	// no Guest apply consent.
	Consents *consent.Service

	// CommunicationPreferences keeps the Announcement preference
	// (docs/communication-preferences.md). Nil or off (COMMUNICATION_PREFERENCE_KEY unset)
	// answers its routes 503.
	CommunicationPreferences *commpref.Service

	// Audience resolves a News send's audience for SkyMail's service account
	// (docs/news-audience.md). Nil or off answers POST /v1/audiences/resolve
	// 503.
	Audience *commpref.Audience

	// Recipients answers SkyMail's service account, for the recipients a
	// sender chose, which are a person and whether the mail may go to each
	// (docs/news-audience.md, "Recipients"). Nil or off answers POST
	// /internal/v1/audiences/recipients:resolve 503.
	Recipients *commpref.Recipients

	// TrackingWithdrawalMetrics are the tracking withdrawals' counters and
	// backlog (docs/communication-preferences.md, "Tracking"), served on
	// /v1/metrics. Nil leaves them out.
	TrackingWithdrawalMetrics interface{ Prometheus() string }

	// MediaDecodeMetrics are the media decode budget's state and counters
	// (docs/media-lifecycle.md, "Decode budget"), served on /v1/metrics.
	// Nil leaves them out.
	MediaDecodeMetrics interface{ Prometheus() string }

	// MemoryMetrics are core's soft memory limit, its container's limit and
	// the memory the Go runtime holds (docs/memory-limit.md), served on
	// /v1/metrics. Nil leaves them out.
	MemoryMetrics interface{ Prometheus() string }

	// Readiness is GET /v1/ready's database and shutdown check, asked
	// before the account access gate (internal/health). Nil leaves the
	// gate's check alone. /v1/health (liveness) asks nothing.
	Readiness *health.Readiness
}

func New(deps Deps) *fiber.App {
	if deps.ParseToken == nil {
		deps.ParseToken = func(string) (authn.Identity, error) {
			return authn.Identity{}, authn.ErrInvalidToken
		}
	}
	trustedProxies := deps.TrustedProxies
	if trustedProxies.Empty() {
		trustedProxies = clientip.Default()
	}
	// The edge proxy discards a caller-supplied `X-Forwarded-For` and writes
	// its own, so the header is only worth reading when the connection came
	// from one of those proxies. Fiber makes that decision for c.IP() from the
	// same ranges the handlers use, and validation keeps c.IP() from handing
	// back a raw header value.
	app := fiber.New(fiber.Config{
		ErrorHandler: handlers.ErrorHandler,
		// Fiber's 4 MiB default rejected media the service accepts. Leave room
		// for the multipart envelope around the largest allowed file.
		BodyLimit: media.MaxUploadBytes + 1<<20,
		// Fiber's 4 KiB read buffer also caps the request headers, and an
		// admin's bearer (past 3 KB) plus the browser's .yildizskylab.com
		// cookies ran over it: every call answered 431. 16 KiB matches
		// Node's default, which the Next.js frontends already accept.
		ReadBufferSize:     16 << 10,
		TrustProxy:         true,
		TrustProxyConfig:   fiber.TrustProxyConfig{Proxies: trustedProxies.Proxies()},
		ProxyHeader:        fiber.HeaderXForwardedFor,
		EnableIPValidation: true,
	})
	app.Use(recover.New())
	app.Use(requestid.New())

	me := handlers.NewMeHandler(deps.Users, deps.Media)
	ident := handlers.NewIdentityHandler(deps.Identity)
	teams := handlers.NewTeamHandler(deps.Identity)
	events := handlers.NewEventHandler(deps.Events)
	seasons := handlers.NewSeasonHandler(deps.Seasons, deps.Events)
	schedule := handlers.NewScheduleHandler(deps.Events)
	tickets := handlers.NewTicketHandler(deps.Tickets)
	competitors := handlers.NewCompetitorHandler(deps.Competitors)
	mediaH := handlers.NewMediaHandler(deps.Media).TrustProxies(trustedProxies)
	urls := handlers.NewURLHandler(deps.URLs, deps.ParseToken, deps.URLAttributionGuard).TrustProxies(trustedProxies)
	var wakeUserSync middlewares.Waker
	if deps.UserSync != nil {
		wakeUserSync = deps.UserSync
	}
	jit := middlewares.NewJIT(deps.Users, wakeUserSync)
	uploadLimiter := deps.MediaUploadLimiter
	if uploadLimiter == nil {
		uploadLimiter = media.NewUploadLimiter(media.DefaultUploadLimits(), time.Now)
	}
	// One budget per person across every single-step upload route. Direct
	// upload (/v1/uploads) stays off it: it has its own budget, kept by the
	// media service (decision Q23), since its bytes never pass through core.
	limitUploads := handlers.LimitMediaUploads(uploadLimiter)
	var certs *handlers.CertificateHandler
	if deps.Certificates != nil {
		certs = handlers.NewCertificateHandler(deps.Certificates)
	}
	var pass *handlers.SkyPassHandler
	if deps.SkyPass != nil {
		pass = handlers.NewSkyPassHandler(deps.SkyPass, deps.Tickets)
	}

	app.Get("/v1/health", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})
	if deps.AccountAccessMetrics != nil || deps.AccountErasureMetrics != nil || deps.GroupOverage != nil || deps.GuestApplyMetrics != nil ||
		deps.GuestCheckInMetrics != nil || deps.MediaCDNPurgeMetrics != nil || deps.AuthzRoleMetrics != nil || deps.RetentionMetrics != nil ||
		deps.SkyPassWalletMetrics != nil || deps.MediaDecodeMetrics != nil || deps.MemoryMetrics != nil || deps.TeamMailMetrics != nil ||
		deps.TeamDelegationMetrics != nil || deps.TrackingWithdrawalMetrics != nil || deps.UserSync != nil || deps.Readiness != nil ||
		deps.EventsMetrics != nil || deps.ManagementTerms != nil {
		app.Get("/v1/metrics", func(c fiber.Ctx) error {
			c.Set(fiber.HeaderCacheControl, "no-store")
			c.Set(fiber.HeaderContentType, "text/plain; version=0.0.4; charset=utf-8")
			text := deps.AccountAccessMetrics.Prometheus()
			if deps.AccountErasureMetrics != nil {
				text += deps.AccountErasureMetrics.Prometheus()
			}
			text += deps.GroupOverage.Prometheus()
			text += deps.GuestApplyMetrics.Prometheus()
			if deps.GuestCheckInMetrics != nil {
				text += deps.GuestCheckInMetrics.Prometheus()
			}
			if deps.MediaCDNPurgeMetrics != nil {
				text += deps.MediaCDNPurgeMetrics.Prometheus()
			}
			text += deps.AuthzRoleMetrics.Prometheus()
			if deps.RetentionMetrics != nil {
				text += deps.RetentionMetrics.Prometheus()
			}
			if deps.SkyPassWalletMetrics != nil {
				text += deps.SkyPassWalletMetrics.Prometheus()
			}
			if deps.MediaDecodeMetrics != nil {
				text += deps.MediaDecodeMetrics.Prometheus()
			}
			if deps.MemoryMetrics != nil {
				text += deps.MemoryMetrics.Prometheus()
			}
			if deps.TeamMailMetrics != nil {
				text += deps.TeamMailMetrics.Prometheus()
			}
			if deps.ManagementTerms != nil {
				text += deps.ManagementTerms.Prometheus()
			}
			if deps.TeamDelegationMetrics != nil {
				text += deps.TeamDelegationMetrics.Prometheus()
			}
			if deps.TrackingWithdrawalMetrics != nil {
				text += deps.TrackingWithdrawalMetrics.Prometheus()
			}
			if deps.UserSync != nil {
				text += deps.UserSync.Prometheus()
			}
			if deps.EventsMetrics != nil {
				text += deps.EventsMetrics.Prometheus()
			}
			text += deps.Readiness.Prometheus()
			return c.SendString(text)
		})
	}
	app.Get("/v1/ready", func(c fiber.Ctx) error {
		if err := deps.Readiness.Check(c.Context()); err != nil {
			c.Set(fiber.HeaderCacheControl, "no-store")
			c.Set(fiber.HeaderRetryAfter, "1")
			return fiber.ErrServiceUnavailable
		}
		// The container's health check (core-backend healthcheck) asks
		// with gate=skip: Swarm has one check, which restarts a task that
		// fails it, and restarting core for a gate Redis that is down fixes
		// nothing (core cannot start without it) while it takes down the
		// routes that need no Redis. No route is added for it.
		if deps.AccountAccessGate != nil && c.Query("gate") != "skip" {
			if err := deps.AccountAccessGate.Ready(c.Context()); err != nil {
				deps.AccountAccessMetrics.RecordReadinessFailure()
				c.Set(fiber.HeaderCacheControl, "no-store")
				c.Set(fiber.HeaderRetryAfter, "1")
				return fiber.ErrServiceUnavailable
			}
		}
		return c.SendStatus(fiber.StatusNoContent)
	})
	app.Get("/v1/go/:alias/qr", urls.QR)
	if certs != nil {
		// These routes are unauthenticated, so the budget has to follow the
		// person holding the certificate link (perClientLimit).
		publicCertificateLimit := perClientLimit(trustedProxies)
		app.Get("/v1/go/c/:serial", publicCertificateLimit, certs.PublicPage)
		app.Get("/c/:serial", publicCertificateLimit, certs.PublicPage)
		app.Get("/v1/public/certificates/:serial", publicCertificateLimit, certs.Verify)
		app.Get("/v1/certificates/verify/:serial/pdf", publicCertificateLimit, certs.Download)
		app.Get("/v1/certificates/verify/:serial/qr", publicCertificateLimit, certs.QR)
		app.Get("/v1/certificates/verify/:serial", publicCertificateLimit, certs.Verify)
	}
	if pass != nil {
		app.Get("/v1/skypass/jwks", pass.JWKS)
	}
	if deps.SelfDeletion != nil {
		selfDeletion := handlers.NewAccountDeletionHandler(deps.SelfDeletion, deps.ParseSelfDeleteContext, deps.ParseSelfDeleteSudo, deps.ParseSelfDeleteBearer)
		app.Post("/v1/account-deletion-requests/self", selfDeletion.Begin)
		app.Get("/v1/account-deletion-requests/status", selfDeletion.Status)
		app.Post("/v1/account-deletion-requests/status/retry", selfDeletion.Retry)
	}
	// Guest apply takes a token but does not require one (docs/guest-apply.md):
	// an invalid token leaves the request anonymous, so the route stays ahead
	// of Bearer, which would answer it 401. A valid one meets the same account
	// access gate and Group overage step as on every other route.
	parseToken := authn.WithServiceProducts(deps.ParseToken, deps.ServiceClients)
	guestLimits := handlers.DefaultGuestApplyLimits()
	if deps.GuestApplyPublicIPLimit != "" {
		guestLimits.PublicIPMode = deps.GuestApplyPublicIPLimit
	}
	guestApply := handlers.NewGuestApply(trustedProxies, deps.GuestApplyMetrics, guestLimits, log.Default())
	guestApplyRoute := []any{
		middlewares.OptionalBearer(parseToken),
		middlewares.AccountAccessGate(deps.AccountAccessGate, deps.AccountAccessMetrics),
		middlewares.GroupOverage(deps.GroupOverage),
	}
	for _, limit := range guestApply.Limits() {
		guestApplyRoute = append(guestApplyRoute, limit)
	}
	// Contact consents (docs/contact-consents.md): the confirm and withdraw
	// pages need no sign-in, the signed token in the link is the permission.
	// Their budget follows the opener's address. A withdrawal has a budget
	// of its own, larger: a mail provider's RFC 8058 one-click POSTs come
	// from a few of its addresses for all its users, and pages opened from
	// the same address must not use it up. A forged token costs one HMAC.
	consents := handlers.NewConsentHandler(deps.Consents)
	consentPageLimit := perClientLimit(trustedProxies)
	app.Get(consent.WithdrawPath, consentPageLimit, consents.WithdrawPage)
	app.Post(consent.WithdrawPath, perClientLimitOf(trustedProxies, consentWithdrawLimit), consents.Withdraw)
	app.Get(consent.ConfirmPath, consentPageLimit, consents.ConfirmPage)
	app.Post(consent.ConfirmPath, consentPageLimit, consents.Confirm)
	guestApplyRoute = append(guestApplyRoute, consents.GuestApplyConsents)
	// The unsubscribe link of every News mail (docs/communication-preferences.md):
	// no sign-in, the signed token in the link is the permission. The same
	// budgets as the consent pages, for the same reasons (a mail provider's
	// one-click POSTs come from a few of its addresses).
	communicationPreferences := handlers.NewCommunicationPreferenceHandler(deps.CommunicationPreferences)
	app.Get(commpref.UnsubscribePath, perClientLimit(trustedProxies), communicationPreferences.UnsubscribePage)
	app.Post(commpref.UnsubscribePath, perClientLimitOf(trustedProxies, consentWithdrawLimit), communicationPreferences.Unsubscribe)
	app.Post("/v1/events/:eventId/applications/guest", guestApply.Observe, append(guestApplyRoute, tickets.ApplyGuest)...)
	// A read link opens a private Media without a sign-in: the token in it is
	// the permission (docs/media-lifecycle.md). The budget follows the
	// opener's address, like the public certificate routes.
	app.Get("/v1/media/:id/content", perClientLimit(trustedProxies), mediaH.Content)
	// The internal routes answer the public edge 404 before any token is
	// read (ADR-0016); the token checks behind stay the boundary.
	app.Use(middlewares.InternalRoutePrefix, middlewares.InternalRouteGuard())
	app.Use(middlewares.Bearer(parseToken))
	app.Use(middlewares.AccountAccessGate(deps.AccountAccessGate, deps.AccountAccessMetrics))
	// SkyMail's service account with communication-preferences:read: whom a
	// send may track (docs/communication-preferences.md, "Tracking"). Before
	// the Group overage step and JIT: a service account has neither.
	app.Post(handlers.TrackingResolvePath, handlers.NewTrackingResolveHandler(deps.Audience).Resolve)
	// SkyMail's service account with audience:read: which recipients a
	// sender chose are a person, and whether the mail may go to each (ADR
	// 0072 karar 3). Here for the same reason.
	app.Post(handlers.RecipientsResolvePath, handlers.NewRecipientsResolveHandler(deps.Recipients).Resolve)
	app.Get("/v1/go/:alias", urls.Redirect)
	app.Get("/v1/go/:alias/:channel", urls.RedirectChannel)
	// Before JIT and every product route: a Group overage token gets its
	// Groups, or the request stops here. The short-link hops above use no
	// Group and keep working while Keycloak is unreachable.
	app.Use(middlewares.GroupOverage(deps.GroupOverage))
	app.Use(jit.Handle)

	if pass != nil {
		app.Post("/v1/skypass/card-bind", pass.BindCard)
		app.Delete("/v1/skypass/card-bind", pass.UnbindCard)
		app.Get("/v1/skypass/card", pass.LookupCard)
		app.Post("/v1/skypass/qr", pass.Mint)
		app.Post("/v1/skypass/verify", pass.Verify)
		// SkyPass in Google Wallet (docs/skypass-google-wallet.md): off
		// answers the status with available false and the rest 503.
		app.Get("/v1/skypass/wallet", pass.WalletStatus)
		walletLimit := handlers.SkyPassWalletLimit()
		app.Post("/v1/skypass/wallet/google", walletLimit, pass.GoogleWalletLink)
		app.Delete("/v1/skypass/wallet/google", walletLimit, pass.RevokeGoogleWallet)
	}

	if deps.GithubActivity != nil {
		app.Get("/v1/dashboard/github-activity", handlers.NewGithubActivityHandler(deps.GithubActivity).Get)
	}

	if deps.SubjectRequests != nil {
		requests := handlers.NewSubjectRequestHandler(deps.SubjectRequests)
		app.Get("/v1/subject-requests", requests.List)
		app.Post("/v1/subject-requests", requests.Create)
		app.Get("/v1/subject-requests/:id", requests.Get)
		app.Patch("/v1/subject-requests/:id", requests.Update)
	}

	app.Get("/v1/users/me/consents", consents.Mine)
	app.Post("/v1/users/me/consents", consents.GrantMine)
	app.Delete("/v1/users/me/consents/:purpose", consents.WithdrawMine)
	app.Get("/v1/users/me/communication-preferences", communicationPreferences.Mine)
	app.Put("/v1/users/me/communication-preferences", communicationPreferences.Update)
	// A product's service account with consent:record; SkyMail's with
	// consent:audience:read.
	app.Post("/v1/consents", consents.Record)
	app.Post("/v1/consents/withdrawals", consents.WithdrawForAddress)
	app.Post("/v1/consents/lookup", consents.Lookup)
	app.Get("/v1/consents/audience", consents.Audience)
	app.Post("/v1/consents/renewal-requests", consents.RequestRenewal)
	// SkyMail's service account with audience:read: who a News send goes to.
	app.Post("/v1/audiences/resolve", handlers.NewAudienceHandler(deps.Audience).Resolve)

	app.Get("/v1/users/me", me.GetMe)
	app.Put("/v1/users/me", me.PutMe)
	app.Patch("/v1/users/me", me.PatchMe)
	if deps.Authz != nil {
		capabilities := handlers.NewCapabilitiesHandler(deps.Authz, editableSitesSource(deps.EditableSites))
		if deps.ManagedTeams != nil {
			capabilities.WithManagedTeams(deps.ManagedTeams)
		}
		if deps.ManagementTerms != nil {
			capabilities.WithExpiredMemberships(deps.ManagementTerms)
		}
		app.Get("/v1/users/me/capabilities", capabilities.Get)
	}
	app.Get("/v1/users/me/managed-groups", ident.ManagedGroups)
	// The Team audit record, for the audit:read role (docs/team-audit.md).
	app.Get("/v1/team-audit-records", ident.TeamAuditRecords)
	app.Post("/v1/users/me/profile-picture", limitUploads, me.ProfilePicture)
	app.Delete("/v1/users/me/profile-picture", me.DeleteProfilePicture)
	app.Get("/v1/users", ident.ListUsers)
	app.Post("/v1/users", ident.CreateUser)
	app.Get("/v1/users/:id", ident.GetUser)
	app.Patch("/v1/users/:id", ident.PatchUser)
	app.Delete("/v1/users/:id", ident.DeleteUser)
	app.Post("/v1/users/:id/logout", ident.LogoutAllSessions)
	app.Post("/v1/users/:id/client-roles", ident.AddUserExtraRole)
	app.Delete("/v1/users/:id/client-roles", ident.RemoveUserExtraRole)
	app.Get("/v1/client-roles", ident.ListClientRoles)

	app.Get("/v1/groups", ident.ListGroups)
	app.Post("/v1/groups", ident.CreateGroup)
	app.Get("/v1/groups/:groupId", ident.GetGroup)
	app.Patch("/v1/groups/:groupId", ident.UpdateGroup)
	app.Delete("/v1/groups/:groupId", ident.DeleteGroup)
	app.Put("/v1/groups/:groupId/settings", ident.SetTeamSettings)
	app.Post("/v1/groups/:groupId/children", ident.CreateSubTeam)
	app.Put("/v1/groups/:groupId/code", ident.ChangeTeamCode)
	app.Get("/v1/groups/:groupId/leaders", ident.TeamLeaders)
	app.Post("/v1/groups/:groupId/leaders", ident.AddTeamLeader)
	app.Delete("/v1/groups/:groupId/leaders/:userId", ident.RemoveTeamLeader)
	app.Post("/v1/groups/:groupId/move", ident.MoveTeam)
	app.Get("/v1/groups/:groupId/move-preview", ident.MoveTeamPreview)
	app.Get("/v1/groups/:groupId/members", ident.Members)
	app.Get("/v1/groups/:groupId/member-candidates", ident.MemberCandidates)
	app.Get("/v1/groups/:groupId/member-check/:userId", ident.MemberCheck)
	app.Post("/v1/groups/:groupId/members", ident.AddMember)
	app.Delete("/v1/groups/:groupId/members/:userId", ident.RemoveMember)
	app.Get("/v1/groups/:groupId/client-roles", ident.GroupClientRoles)
	app.Put("/v1/groups/:groupId/client-roles", ident.SetGroupClientRoles)

	// Management membership terms (docs/management-membership-terms.md):
	// served only while they are kept (MANAGEMENT_TERMS_MODE=apply).
	if deps.ManagementTerms != nil && deps.ManagementTerms.Mode() == membershipterm.ModeApply {
		terms := handlers.NewManagementTermsHandler(deps.ManagementTerms)
		app.Get("/v1/management-memberships", terms.List)
		app.Post("/v1/management-memberships/:termId/extend", terms.Extend)
		app.Post("/v1/management-memberships/:termId/handover", terms.Handover)
		app.Delete("/v1/management-memberships/:termId", terms.Remove)
	}

	if deps.AppAssignments {
		apps := handlers.NewAppsHandler(deps.Identity)
		app.Get("/v1/apps", apps.List)
		app.Get("/v1/apps/:clientId/assignments", apps.Assignments)
		app.Post("/v1/apps/:clientId/assignments", apps.Assign)
		app.Delete("/v1/apps/:clientId/assignments", apps.Unassign)
		app.Get("/v1/apps/:clientId/audit", apps.Audit)
		app.Get("/v1/apps/:clientId/targets", apps.Targets)
	}

	if deps.Dashboard != nil {
		app.Get("/v1/dashboard/summary", handlers.NewDashboardHandler(deps.Dashboard).Summary)
	}

	// The team routes are public and each read reaches Keycloak: one budget
	// per client address for the three (perClientLimit).
	teamsLimit := perClientLimit(trustedProxies)
	app.Get("/v1/teams", teamsLimit, teams.List)
	app.Get("/v1/teams/:team/members", teamsLimit, teams.Members)
	app.Get("/v1/teams/:team/leaders", teamsLimit, teams.Leaders)

	app.Get("/v1/events", events.List)
	app.Get("/v1/events/active", events.ListActive)
	app.Post("/v1/events", events.Create)
	app.Get("/v1/events/:id", events.Get)
	app.Put("/v1/events/:id", events.Update)
	app.Patch("/v1/events/:id", events.Update)
	app.Delete("/v1/events/:id", events.Delete)
	app.Post("/v1/events/:id/restore", events.Restore)
	app.Post("/v1/events/:id/publish", events.Publish)
	app.Post("/v1/events/:id/unpublish", events.Unpublish)
	app.Post("/v1/events/:id/cancel", events.Cancel)
	app.Post("/v1/events/:id/uncancel", events.Uncancel)
	app.Post("/v1/events/:id/copy", events.Copy)
	if deps.FeedbackInvites != nil && deps.Authz != nil {
		feedbackH := handlers.NewFeedbackHandler(deps.Events, deps.Authz, deps.FeedbackInvites)
		app.Get("/v1/events/:id/feedback", feedbackH.Counts)
		if deps.FeedbackSender != nil {
			feedbackH.SendWith(deps.FeedbackSender)
			app.Post("/v1/events/:id/feedback/send", feedbackH.SendEventNow)
			app.Post("/v1/sessions/:id/feedback/send", feedbackH.SendSessionNow)
		}
		if deps.FeedbackReminders != nil {
			feedbackH.RemindWith(deps.FeedbackReminders)
			app.Post("/v1/events/:id/feedback/close", feedbackH.CloseEvent)
			app.Post("/v1/sessions/:id/feedback/close", feedbackH.CloseSession)
		}
	}
	app.Post("/v1/events/:id/images", events.AddImages)
	app.Delete("/v1/events/:id/images", events.RemoveImages)
	app.Post("/v1/events/:id/files", events.AddFiles(event.Files))
	app.Delete("/v1/events/:id/files", events.RemoveFiles(event.Files))
	app.Put("/v1/events/:id/files/order", events.OrderFiles(event.Files))
	app.Post("/v1/events/:id/videos", events.AddFiles(event.Videos))
	app.Delete("/v1/events/:id/videos", events.RemoveFiles(event.Videos))
	app.Put("/v1/events/:id/videos/order", events.OrderFiles(event.Videos))
	app.Put("/v1/events/:id/videos/:mediaId/poster", events.SetVideoPoster)
	app.Delete("/v1/events/:id/videos/:mediaId/poster", events.ClearVideoPoster)
	app.Get("/v1/events/:eventId/days", schedule.ListDays)
	app.Post("/v1/events/:eventId/applications/me", tickets.Apply)
	app.Post("/v1/events/:eventId/applications/users/:userId", tickets.ApplyForOther)
	app.Get("/v1/events/:eventId/assignable-users", tickets.ListAssignableUsers)
	app.Get("/v1/events/:eventId/tickets", tickets.ListByEvent)
	app.Get("/v1/door/events", tickets.ListDoorEvents)
	app.Get("/v1/events/:eventId/door-attendees", tickets.SearchDoorAttendees)
	if deps.EventMail != nil {
		mailList := handlers.NewEventMailHandler(deps.EventMail)
		app.Post("/v1/events/:eventId/mail-list", mailList.Sync)
	}
	if certs != nil {
		app.Get("/v1/events/:eventId/certificates", certs.ListByEvent)
		app.Get("/v1/events/:eventId/certificates/summary", certs.Summary)
		app.Get("/v1/events/:eventId/certificates/template", certs.Resolve)
		app.Post("/v1/events/:eventId/certificates/preview", certs.PreviewEvent)
		app.Get("/v1/events/:eventId/certificate-batches", certs.ListBatches)
		app.Post("/v1/events/:eventId/certificates/issue", certs.Issue)
		app.Post("/v1/events/:eventId/certificates/finalize", certs.Finalize)
		app.Post("/v1/events/:eventId/certificates/recompute", certs.Recompute)
		app.Get("/v1/certificates/me", certs.Mine)
		app.Get("/v1/certificate-templates", certs.ListTemplates)
		app.Post("/v1/certificate-templates", certs.CreateTemplate)
		app.Get("/v1/certificate-templates/:id", certs.GetTemplate)
		app.Put("/v1/certificate-templates/:id", certs.UpdateTemplate)
		app.Post("/v1/certificate-templates/:id/publish", certs.PublishTemplate)
		app.Post("/v1/certificate-templates/:id/preview", certs.PreviewTemplate)
		app.Get("/v1/certificate-template-bindings", certs.ListBindings)
		app.Put("/v1/certificate-template-bindings", certs.SetBinding)
		app.Delete("/v1/certificate-template-bindings/:scope/:scopeKey", certs.ClearBinding)
		app.Get("/v1/certificate-batches/:id", certs.GetBatch)
		app.Post("/v1/certificate-batches/:id/retry", certs.RetryBatch)
		app.Post("/v1/certificates/:serial/revoke", certs.Revoke)
		app.Post("/v1/certificates/:serial/reissue", certs.Reissue)
	}
	app.Get("/v1/events/:eventId/competitors", competitors.ListByEvent)
	app.Get("/v1/events/:eventId/competitors/winner", competitors.Winner)
	app.Delete("/v1/events/:eventId/season", seasons.UnassignEvent)

	app.Get("/v1/seasons", seasons.List)
	app.Post("/v1/seasons", seasons.Create)
	app.Get("/v1/seasons/:id", seasons.Get)
	app.Put("/v1/seasons/:id", seasons.Update)
	app.Patch("/v1/seasons/:id", seasons.Update)
	app.Delete("/v1/seasons/:id", seasons.Delete)
	app.Post("/v1/seasons/:id/restore", seasons.Restore)
	app.Get("/v1/seasons/:id/events", seasons.ListEvents)
	app.Post("/v1/seasons/:id/events/:eventId", seasons.AssignEvent)

	app.Post("/v1/event-days", schedule.CreateDay)
	app.Get("/v1/event-days/:id", schedule.GetDay)
	app.Put("/v1/event-days/:id", schedule.UpdateDay)
	app.Delete("/v1/event-days/:id", schedule.DeleteDay)
	app.Post("/v1/event-days/:id/restore", schedule.RestoreDay)
	app.Get("/v1/event-days/:id/sessions", schedule.ListSessions)
	app.Get("/v1/event-days/:id/current-session", schedule.CurrentSession)

	app.Post("/v1/sessions", schedule.CreateSession)
	app.Get("/v1/sessions/:id/qr", schedule.SessionQR)
	app.Get("/v1/sessions/:id", schedule.GetSession)
	app.Put("/v1/sessions/:id", schedule.UpdateSession)
	app.Delete("/v1/sessions/:id", schedule.DeleteSession)
	app.Post("/v1/sessions/:id/restore", schedule.RestoreSession)

	app.Get("/v1/tickets/me", tickets.Mine)
	app.Get("/v1/tickets/user/:userId/event/:eventId", tickets.ByUserEvent)
	app.Get("/v1/tickets/:id", tickets.Get)
	app.Get("/v1/tickets", tickets.List)
	// The forms service's account with ticket:forms reports each answer to its
	// forms; an accepted answer to an Event's form writes its Ticket
	// (docs/form-response-tickets.md).
	app.Post("/v1/forms/:formId/responses", tickets.RecordFormResponse)
	app.Post("/v1/tickets/:ticketId/sessions/:sessionId/check-in", tickets.CheckIn)
	app.Post("/v1/sessions/:sessionId/check-in/me", tickets.CheckInMe)
	// Guest check-in takes no sign-in; failures are budgeted per address, and
	// the door QR caps how many guests one token lets in
	// (docs/guest-self-check-in.md).
	doorQRLimits := handlers.DefaultDoorQRLimits()
	if deps.DoorQRLimits != nil {
		doorQRLimits = *deps.DoorQRLimits
	}
	app.Post("/v1/sessions/:sessionId/check-in/guest", doorQRLimits.GuestCheckInLimit(trustedProxies), tickets.CheckInGuest)
	app.Post("/v1/sessions/:sessionId/door-qr", doorQRLimits.MintLimit(), tickets.MintDoorQR)
	app.Post("/v1/sessions/:sessionId/check-in/resolve", tickets.ResolveAndCheckIn)
	app.Get("/v1/sessions/:sessionId/check-ins", tickets.DoorActivity)
	if pass != nil {
		app.Post("/v1/sessions/:sessionId/check-in/skypass", pass.CheckInSession)
	}

	app.Get("/v1/competitors", competitors.List)
	app.Get("/v1/competitors/me", competitors.Mine)
	app.Get("/v1/competitors/leaderboard/team/:ownerTeam", competitors.LeaderboardByTeam)
	app.Get("/v1/competitors/leaderboard/season/:seasonId/team/:ownerTeam", competitors.LeaderboardBySeason)
	app.Get("/v1/competitors/user/:userId", competitors.ListByUser)
	app.Get("/v1/competitors/team/:ownerTeam", competitors.ListByOwnerTeam)
	app.Post("/v1/competitors", competitors.Create)
	app.Get("/v1/competitors/:id", competitors.Get)
	app.Put("/v1/competitors/:id", competitors.Update)
	app.Delete("/v1/competitors/:id", competitors.Delete)
	app.Post("/v1/competitors/:id/reinstate", competitors.Reinstate)

	// The competition module (ADR-0068, docs/competitions.md); nil serves
	// none of its routes.
	if deps.Competitions != nil {
		comps := handlers.NewCompetitionHandler(deps.Competitions)
		app.Get("/v1/events/:eventId/competitions", comps.ListByEvent)
		app.Post("/v1/events/:eventId/competitions", comps.Create)
		app.Get("/v1/competitions/:id", comps.Get)
		app.Patch("/v1/competitions/:id", comps.Update)
		for _, action := range []string{"open", "start", "close", "reopen", "cancel"} {
			app.Post("/v1/competitions/:id/"+action, comps.Transition(action))
		}
		app.Post("/v1/competitions/:id/archive", comps.Archive)
		app.Post("/v1/competitions/:id/restore", comps.Restore)
		app.Get("/v1/competitions/:id/entrants", comps.Entrants)
		app.Post("/v1/competitions/:id/entrants", comps.AddEntrants)
		app.Get("/v1/competitions/:id/entrant-candidates", comps.EntrantCandidates)
		app.Post("/v1/competitions/:id/scores", comps.SetScore)
		app.Get("/v1/competitions/:id/entries", comps.Entries)
		app.Post("/v1/competitions/:id/entries/:entryId/revoke", comps.RevokeEntry)
		app.Get("/v1/competitions/:id/leaderboard", comps.Leaderboard)
	}

	app.Post("/v1/media", limitUploads, mediaH.Upload)
	// The address lookup: a product's service account asks which Media the
	// addresses its content stores name (docs/media-lifecycle.md).
	app.Post("/v1/media/lookup", handlers.LimitMediaLookups(deps.MediaLookupsPerMinute), mediaH.LookUp)
	app.Get("/v1/media", mediaH.List)
	app.Get("/v1/media/:id", mediaH.Get)
	app.Delete("/v1/media/:id", mediaH.Delete)
	app.Post("/v1/media/:id/restore", mediaH.Restore)
	// The service attach API: another product's service account links Media
	// to its own records (docs/media-lifecycle.md).
	app.Post("/v1/media/:id/attachments", mediaH.Attach)
	// The owning product's five-minute read link to one of its private Media.
	app.Post("/v1/media/:id/links", mediaH.IssueReadLink)
	app.Delete("/v1/media/:id/attachments/:attachmentId", mediaH.Detach)
	// Direct upload of large Media: the browser sends the parts straight to
	// storage (docs/media-lifecycle.md).
	app.Post("/v1/uploads", mediaH.StartUpload)
	app.Post("/v1/uploads/:id/parts", mediaH.UploadParts)
	app.Post("/v1/uploads/:id/complete", mediaH.CompleteUpload)

	app.Post("/v1/urls", urls.Create)
	app.Get("/v1/urls", urls.ListMine)
	app.Get("/v1/urls/all", urls.ListAll)
	app.Get("/v1/urls/availability", urls.Availability)
	app.Get("/v1/urls/forms/:formId", urls.FormLink)
	app.Put("/v1/urls/forms/:formId", urls.EnsureFormLink)
	app.Patch("/v1/urls/forms/:formId", urls.RenameFormLink)
	app.Get("/v1/urls/forms/:formId/stats", urls.FormStats)
	app.Get("/v1/urls/:id/hits", urls.ListHits)
	app.Patch("/v1/urls/:id", urls.Update)
	app.Delete("/v1/urls/:id", urls.Delete)
	app.Post("/v1/urls/:id/restore", urls.Restore)

	return app
}

// perClientLimit is the budget of an unauthenticated route: 120 requests a
// minute for each client address. Keying on the default c.IP() would put
// every visitor behind the edge proxy in one bucket and let a single caller
// exhaust it for everyone. An address that cannot be resolved shares one
// bucket on purpose: unattributable traffic is limited together rather than
// exempted.
func perClientLimit(trustedProxies clientip.Ranges) fiber.Handler {
	return perClientLimitOf(trustedProxies, 120)
}

// consentWithdrawLimit is a withdrawal's budget a minute per client address
// (one-click POSTs of a mail provider share a few addresses).
const consentWithdrawLimit = 600

// perClientLimitOf is perClientLimit with max requests a minute, in a budget
// of its own.
func perClientLimitOf(trustedProxies clientip.Ranges, max int) fiber.Handler {
	return limiter.New(limiter.Config{
		Max:        max,
		Expiration: time.Minute,
		KeyGenerator: func(c fiber.Ctx) string {
			return clientip.FromCtx(c, trustedProxies)
		},
	})
}

// editableSitesSource keeps an unset Deps.EditableSites a nil interface, so
// the capabilities handler neither asks it nor logs for it.
func editableSitesSource(sites *editablesites.Sites) handlers.EditableSitesSource {
	if sites == nil {
		return nil
	}
	return sites
}
