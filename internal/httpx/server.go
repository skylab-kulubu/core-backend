package httpx

import (
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
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
	Users        user.Service
	Identity     identity.Service
	Events       event.Service
	Seasons      season.Service
	Tickets      ticket.Service
	Competitors  competitor.Service
	Media        media.Service
	URLs         shorturl.Service
	Certificates certificate.Service
	SkyPass      skypass.Service
	Mail         mail.Mailer
	ParseToken   func(string) (authn.Identity, error)
}

func New(deps Deps) *fiber.App {
	if deps.ParseToken == nil {
		deps.ParseToken = func(string) (authn.Identity, error) {
			return authn.Identity{}, authn.ErrInvalidToken
		}
	}
	app := fiber.New(fiber.Config{ErrorHandler: handlers.ErrorHandler})
	app.Use(recover.New())

	me := handlers.NewMeHandler(deps.Users, deps.Media)
	ident := handlers.NewIdentityHandler(deps.Identity)
	teams := handlers.NewTeamHandler(deps.Identity)
	events := handlers.NewEventHandler(deps.Events)
	seasons := handlers.NewSeasonHandler(deps.Seasons, deps.Events)
	schedule := handlers.NewScheduleHandler(deps.Events)
	tickets := handlers.NewTicketHandler(deps.Tickets)
	competitors := handlers.NewCompetitorHandler(deps.Competitors)
	mediaH := handlers.NewMediaHandler(deps.Media)
	urls := handlers.NewURLHandler(deps.URLs)
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
	app.Get("/v1/go/:alias/qr", urls.QR)
	app.Get("/v1/go/:alias", urls.Redirect)
	if certs != nil {
		app.Get("/v1/certificates/verify/:serial/pdf", certs.Download)
		app.Get("/v1/certificates/verify/:serial/qr", certs.QR)
		app.Get("/v1/certificates/verify/:serial", certs.Verify)
	}
	if pass != nil {
		app.Get("/v1/skypass/jwks", pass.JWKS)
	}
	app.Use(middlewares.Bearer(deps.ParseToken))
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
	app.Get("/v1/users", ident.ListUsers)
	app.Post("/v1/users", ident.CreateUser)
	app.Get("/v1/users/:id", ident.GetUser)
	app.Delete("/v1/users/:id", ident.DeleteUser)
	app.Post("/v1/users/:id/logout", ident.LogoutAllSessions)
	app.Post("/v1/users/:id/client-roles", ident.AddUserExtraRole)
	app.Delete("/v1/users/:id/client-roles", ident.RemoveUserExtraRole)

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
	app.Post("/v1/events/:id/images", events.AddImages)
	app.Delete("/v1/events/:id/images", events.RemoveImages)
	app.Get("/v1/events/:eventId/days", schedule.ListDays)
	app.Post("/v1/events/:eventId/applications/me", tickets.Apply)
	app.Post("/v1/events/:eventId/applications/guest", tickets.ApplyGuest)
	app.Get("/v1/events/:eventId/tickets", tickets.ListByEvent)
	if certs != nil {
		app.Get("/v1/events/:eventId/certificates", certs.ListByEvent)
		app.Post("/v1/events/:eventId/certificates/issue", certs.Issue)
		app.Post("/v1/events/:eventId/certificates/recompute", certs.Recompute)
		app.Get("/v1/certificates/me", certs.Mine)
		app.Post("/v1/certificates/:serial/revoke", certs.Revoke)
	}
	app.Get("/v1/events/:eventId/competitors", competitors.ListByEvent)
	app.Get("/v1/events/:eventId/competitors/winner", competitors.Winner)
	app.Delete("/v1/events/:eventId/season", seasons.UnassignEvent)

	app.Get("/v1/seasons", seasons.List)
	app.Post("/v1/seasons", seasons.Create)
	app.Get("/v1/seasons/:id", seasons.Get)
	app.Put("/v1/seasons/:id", seasons.Update)
	app.Delete("/v1/seasons/:id", seasons.Delete)
	app.Get("/v1/seasons/:id/events", seasons.ListEvents)
	app.Post("/v1/seasons/:id/events/:eventId", seasons.AssignEvent)

	app.Post("/v1/event-days", schedule.CreateDay)
	app.Get("/v1/event-days/:id", schedule.GetDay)
	app.Put("/v1/event-days/:id", schedule.UpdateDay)
	app.Delete("/v1/event-days/:id", schedule.DeleteDay)
	app.Get("/v1/event-days/:id/sessions", schedule.ListSessions)
	app.Get("/v1/event-days/:id/current-session", schedule.CurrentSession)

	app.Post("/v1/sessions", schedule.CreateSession)
	app.Get("/v1/sessions/:id/qr", schedule.SessionQR)
	app.Get("/v1/sessions/:id", schedule.GetSession)
	app.Put("/v1/sessions/:id", schedule.UpdateSession)
	app.Delete("/v1/sessions/:id", schedule.DeleteSession)

	app.Get("/v1/tickets/me", tickets.Mine)
	app.Get("/v1/tickets/user/:userId/event/:eventId", tickets.ByUserEvent)
	app.Get("/v1/tickets/:id", tickets.Get)
	app.Get("/v1/tickets", tickets.List)
	app.Post("/v1/tickets/:ticketId/sessions/:sessionId/check-in", tickets.CheckIn)
	app.Post("/v1/sessions/:sessionId/check-in/me", tickets.CheckInMe)
	app.Post("/v1/sessions/:sessionId/check-in/guest", tickets.CheckInGuest)
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

	app.Post("/v1/media", mediaH.Upload)
	app.Get("/v1/media", mediaH.List)
	app.Get("/v1/media/:id", mediaH.Get)
	app.Delete("/v1/media/:id", mediaH.Delete)

	app.Post("/v1/urls", urls.Create)
	app.Get("/v1/urls", urls.ListMine)
	app.Get("/v1/urls/all", urls.ListAll)
	app.Patch("/v1/urls/:id", urls.Update)
	app.Delete("/v1/urls/:id", urls.Delete)

	return app
}
