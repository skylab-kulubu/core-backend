package httpx

import (
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventmail"
	"github.com/skylab-kulubu/core-backend/internal/handlers"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/middlewares"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/skypass"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Deps struct {
	Users                  user.Service
	Identity               identity.Service
	Events                 event.Service
	Seasons                season.Service
	Tickets                ticket.Service
	Competitors            competitor.Service
	Media                  media.Service
	URLs                   shorturl.Service
	Certificates           certificate.Service
	SkyPass                skypass.Service
	Mail                   mail.Mailer
	EventMail              eventmail.Service
	ParseToken             func(string) (authn.Identity, error)
	URLAttributionGuard    handlers.URLAttributionGuard
	AccountAccessGate      accessgate.Reader
	AccountAccessMetrics   *accessgate.Metrics
	SelfDeletion           handlers.AccountDeletionService
	ParseSelfDeleteContext func(string, string) (authn.Identity, error)

	// TrustedProxies are the peers allowed to speak for a client through
	// `X-Forwarded-For`. An empty value falls back to clientip.Default().
	TrustedProxies clientip.Ranges
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
		ErrorHandler:       handlers.ErrorHandler,
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
	mediaH := handlers.NewMediaHandler(deps.Media)
	urls := handlers.NewURLHandler(deps.URLs, deps.ParseToken, deps.URLAttributionGuard).TrustProxies(trustedProxies)
	jit := middlewares.NewJIT(deps.Users, deps.Mail)
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
	if deps.AccountAccessMetrics != nil {
		app.Get("/v1/metrics", func(c fiber.Ctx) error {
			c.Set(fiber.HeaderCacheControl, "no-store")
			c.Set(fiber.HeaderContentType, "text/plain; version=0.0.4; charset=utf-8")
			return c.SendString(deps.AccountAccessMetrics.Prometheus())
		})
	}
	app.Get("/v1/ready", func(c fiber.Ctx) error {
		if deps.AccountAccessGate != nil {
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
		// person holding the certificate link. Keying on the default c.IP()
		// would put every visitor behind the edge proxy in one bucket and let
		// a single caller exhaust it for everyone. An address that cannot be
		// resolved shares one bucket on purpose: unattributable traffic is
		// limited together rather than exempted.
		publicCertificateLimit := limiter.New(limiter.Config{
			Max:        120,
			Expiration: time.Minute,
			KeyGenerator: func(c fiber.Ctx) string {
				return clientip.FromCtx(c, trustedProxies)
			},
		})
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
		selfDeletion := handlers.NewAccountDeletionHandler(deps.SelfDeletion, deps.ParseSelfDeleteContext)
		app.Post("/v1/account-deletion-requests/self", selfDeletion.Begin)
		app.Get("/v1/account-deletion-requests/status", selfDeletion.Status)
		app.Post("/v1/account-deletion-requests/status/retry", selfDeletion.Retry)
	}
	app.Post("/v1/events/:eventId/applications/guest", tickets.ApplyGuest)
	app.Use(middlewares.Bearer(deps.ParseToken))
	app.Use(middlewares.AccountAccessGate(deps.AccountAccessGate, deps.AccountAccessMetrics))
	app.Get("/v1/go/:alias", urls.Redirect)
	app.Use(jit.Handle)

	if pass != nil {
		app.Post("/v1/skypass/card-bind", pass.BindCard)
		app.Get("/v1/skypass/card", pass.LookupCard)
		app.Post("/v1/skypass/qr", pass.Mint)
		app.Post("/v1/skypass/verify", pass.Verify)
	}

	app.Get("/v1/users/me", me.GetMe)
	app.Put("/v1/users/me", me.PutMe)
	app.Patch("/v1/users/me", me.PatchMe)
	app.Post("/v1/users/me/profile-picture", me.ProfilePicture)
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
	app.Get("/v1/groups/:groupId/members", ident.Members)
	app.Post("/v1/groups/:groupId/members", ident.AddMember)
	app.Delete("/v1/groups/:groupId/members/:userId", ident.RemoveMember)
	app.Get("/v1/groups/:groupId/client-roles", ident.GroupClientRoles)
	app.Put("/v1/groups/:groupId/client-roles", ident.SetGroupClientRoles)

	app.Get("/v1/teams", teams.List)
	app.Get("/v1/teams/:team/members", teams.Members)
	app.Get("/v1/teams/:team/leaders", teams.Leaders)

	app.Get("/v1/events", events.List)
	app.Get("/v1/events/active", events.ListActive)
	app.Post("/v1/events", events.Create)
	app.Get("/v1/events/:id", events.Get)
	app.Put("/v1/events/:id", events.Update)
	app.Patch("/v1/events/:id", events.Update)
	app.Delete("/v1/events/:id", events.Delete)
	app.Post("/v1/events/:id/restore", events.Restore)
	app.Post("/v1/events/:id/images", events.AddImages)
	app.Delete("/v1/events/:id/images", events.RemoveImages)
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
	app.Post("/v1/tickets/:ticketId/sessions/:sessionId/check-in", tickets.CheckIn)
	app.Post("/v1/sessions/:sessionId/check-in/me", tickets.CheckInMe)
	app.Post("/v1/sessions/:sessionId/check-in/guest", tickets.CheckInGuest)
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

	app.Post("/v1/media", mediaH.Upload)
	app.Get("/v1/media", mediaH.List)
	app.Get("/v1/media/:id", mediaH.Get)
	app.Delete("/v1/media/:id", mediaH.Delete)
	app.Post("/v1/media/:id/restore", mediaH.Restore)

	app.Post("/v1/urls", urls.Create)
	app.Get("/v1/urls", urls.ListMine)
	app.Get("/v1/urls/all", urls.ListAll)
	app.Get("/v1/urls/:id/hits", urls.ListHits)
	app.Patch("/v1/urls/:id", urls.Update)
	app.Delete("/v1/urls/:id", urls.Delete)
	app.Post("/v1/urls/:id/restore", urls.Restore)

	return app
}
