package httpx

import (
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/handlers"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/middlewares"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Deps struct {
	Users       user.Service
	Identity    identity.Service
	Events      event.Service
	Seasons     season.Service
	Tickets     ticket.Service
	Competitors competitor.Service
	Media       media.Service
	URLs        shorturl.Service
	Mail        mail.Mailer
	ParseToken  func(string) (authn.Identity, error)
}

func New(deps Deps) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: handlers.ErrorHandler})
	app.Use(recover.New())

	me := handlers.NewMeHandler()
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

	app.Get("/v1/health", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})
	app.Get("/v1/go/:alias/qr", urls.QR)
	app.Get("/v1/go/:alias", urls.Redirect)
	app.Use(middlewares.Bearer(deps.ParseToken))
	app.Use(jit.Handle)

	app.Get("/v1/users/me", me.GetMe)
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
	app.Post("/v1/events", events.Create)
	app.Get("/v1/events/:id", events.Get)
	app.Put("/v1/events/:id", events.Update)
	app.Delete("/v1/events/:id", events.Delete)
	app.Get("/v1/events/:eventId/days", schedule.ListDays)
	app.Post("/v1/events/:eventId/applications/me", tickets.Apply)
	app.Post("/v1/events/:eventId/applications/guest", tickets.ApplyGuest)
	app.Get("/v1/events/:eventId/tickets", tickets.ListByEvent)
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

	app.Post("/v1/sessions", schedule.CreateSession)
	app.Get("/v1/sessions/:id", schedule.GetSession)
	app.Put("/v1/sessions/:id", schedule.UpdateSession)
	app.Delete("/v1/sessions/:id", schedule.DeleteSession)

	app.Get("/v1/tickets/me", tickets.Mine)
	app.Post("/v1/tickets/:ticketId/event-days/:eventDayId/check-in", tickets.CheckIn)

	app.Get("/v1/competitors", competitors.List)
	app.Get("/v1/competitors/me", competitors.Mine)
	app.Get("/v1/competitors/leaderboard/type/:eventType", competitors.LeaderboardByType)
	app.Get("/v1/competitors/leaderboard/season/:seasonId/type/:eventType", competitors.LeaderboardBySeason)
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
