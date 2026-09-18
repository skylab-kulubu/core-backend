package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"log"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/skypass"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := migrate.Apply(context.Background(), pool); err != nil {
		log.Fatal(err)
	}

	az := authz.NewAuthorizer(authz.DefaultPolicy())
	users := user.NewPostgresStore(pool)
	events := event.NewPostgresStore(pool)
	seasons := season.NewPostgresStore(pool)
	tickets := ticket.NewPostgresStore(pool)
	competitors := competitor.NewPostgresStore(pool)
	certs := certificate.NewPostgresStore(pool)
	mediaStore := media.NewPostgresStore(pool)
	blobs, cdnBase, err := media.BlobAndCDN(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}

	dir := identity.Directory(identity.NewMemory())
	if os.Getenv("KEYCLOAK_URL") != "" {
		if os.Getenv("KEYCLOAK_REALM") == "" || os.Getenv("KEYCLOAK_CLIENT_ID") == "" || os.Getenv("KEYCLOAK_CLIENT_SECRET") == "" {
			log.Fatal("KEYCLOAK_REALM, KEYCLOAK_CLIENT_ID, and KEYCLOAK_CLIENT_SECRET are required with KEYCLOAK_URL")
		}
		dir = identity.NewKeycloak(identity.KeycloakConfig{
			URL:          os.Getenv("KEYCLOAK_URL"),
			Realm:        os.Getenv("KEYCLOAK_REALM"),
			ClientID:     os.Getenv("KEYCLOAK_CLIENT_ID"),
			ClientSecret: os.Getenv("KEYCLOAK_CLIENT_SECRET"),
		})
	}

	parse := func(string) (authn.Identity, error) {
		return authn.Identity{}, authn.ErrInvalidToken
	}
	jwksURL := os.Getenv("KEYCLOAK_JWKS_URL")
	base := strings.TrimRight(os.Getenv("KEYCLOAK_URL"), "/")
	realm := os.Getenv("KEYCLOAK_REALM")
	if parts := strings.SplitN(base, "/realms/", 2); len(parts) == 2 {
		base = parts[0]
		if realm == "" {
			realm = parts[1]
		}
	}
	if jwksURL == "" && base != "" && realm != "" {
		jwksURL = base + "/realms/" + realm + "/protocol/openid-connect/certs"
	}
	issuer := ""
	if base != "" && realm != "" {
		issuer = base + "/realms/" + realm
	} else if i := strings.Index(jwksURL, "/realms/"); i >= 0 {
		host := jwksURL[:i]
		rest := jwksURL[i+len("/realms/"):]
		realmPart, _, _ := strings.Cut(rest, "/")
		if host != "" && realmPart != "" {
			issuer = host + "/realms/" + realmPart
		}
	}
	if jwksURL != "" && issuer == "" {
		log.Fatal("cannot derive JWT issuer; set KEYCLOAK_URL and KEYCLOAK_REALM")
	}
	if jwksURL != "" && issuer != "" {
		v := authn.NewJWKS(jwksURL)
		parse = func(token string) (authn.Identity, error) {
			return authn.ParseAndVerify(token, v.Verify, issuer, authn.ResourceAudience)
		}
	}

	var mailer mail.Mailer
	var sky *mail.SkyMail
	if os.Getenv("SKYMAIL_URL") != "" && (os.Getenv("SKYMAIL_WELCOME_TEMPLATE_ID") != "" || os.Getenv("SKYMAIL_CERTIFICATE_TEMPLATE_ID") != "") {
		kc := strings.TrimRight(os.Getenv("KEYCLOAK_URL"), "/")
		realm := os.Getenv("KEYCLOAK_REALM")
		if parts := strings.SplitN(kc, "/realms/", 2); len(parts) == 2 {
			kc = parts[0]
			if realm == "" {
				realm = parts[1]
			}
		}
		if kc == "" || realm == "" || os.Getenv("KEYCLOAK_CLIENT_ID") == "" || os.Getenv("KEYCLOAK_CLIENT_SECRET") == "" {
			log.Fatal("KEYCLOAK_URL, KEYCLOAK_REALM, KEYCLOAK_CLIENT_ID, and KEYCLOAK_CLIENT_SECRET are required with SKYMAIL_URL")
		}
		sky = &mail.SkyMail{
			BaseURL: os.Getenv("SKYMAIL_URL"),
			Tokens: mail.ClientCredentials{
				TokenURL:     kc + "/realms/" + realm + "/protocol/openid-connect/token",
				ClientID:     os.Getenv("KEYCLOAK_CLIENT_ID"),
				ClientSecret: os.Getenv("KEYCLOAK_CLIENT_SECRET"),
			},
		}
		if raw := os.Getenv("SKYMAIL_WELCOME_TEMPLATE_ID"); raw != "" {
			tid, err := uuid.Parse(raw)
			if err != nil {
				log.Fatal("SKYMAIL_WELCOME_TEMPLATE_ID must be a UUID")
			}
			sky.TemplateID = tid
		}
		if raw := os.Getenv("SKYMAIL_CERTIFICATE_TEMPLATE_ID"); raw != "" {
			tid, err := uuid.Parse(raw)
			if err != nil {
				log.Fatal("SKYMAIL_CERTIFICATE_TEMPLATE_ID must be a UUID")
			}
			sky.CertificateTemplateID = tid
		}
		mailer = sky
	}

	var render certificate.Renderer
	if os.Getenv("GOTENBERG_URL") != "" {
		render = &certificate.Gotenberg{BaseURL: os.Getenv("GOTENBERG_URL")}
	}

	passKey, err := loadSkyPassKey()
	if err != nil {
		log.Fatal(err)
	}

	ticketSvc := ticket.NewService(tickets, events, az, users, dir)
	certSvc := certificate.NewService(certs, tickets, events, users, az, render, sky, os.Getenv("PUBLIC_API_ORIGIN"))
	ticketSvc = ticket.WithSettledCheckIn(ticketSvc, func(ctx context.Context, ticketID uuid.UUID) {
		_, _ = certSvc.RecomputeTicket(ctx, ticketID)
	})

	app := httpx.New(httpx.Deps{
		Users:        user.NewService(users, dir),
		Identity:     identity.NewService(dir, users, az, mailer),
		Events:       event.NewService(events, az, cdnBase),
		Seasons:      season.NewService(seasons, az),
		Tickets:      ticketSvc,
		Competitors:  competitor.NewService(competitors, events, az),
		Media:        media.NewService(mediaStore, blobs, az, cdnBase),
		URLs:         shorturl.NewService(shorturl.NewPostgresStore(pool), az),
		Certificates: certSvc,
		SkyPass:      skypass.NewService(users, az, skypass.NewSigner(passKey, skypass.DefaultTTL)),
		Mail:         mailer,
		ParseToken:   parse,
	})

	addr := os.Getenv("PORT")
	if addr == "" {
		addr = "8080"
	}
	if err := app.Listen(":" + addr); err != nil {
		log.Fatal(err)
	}
}

func loadSkyPassKey() (*rsa.PrivateKey, error) {
	if raw := os.Getenv("SKYPASS_RSA_PRIVATE_KEY"); raw != "" {
		return skypass.ParseRSAPrivateKey([]byte(raw))
	}
	return rsa.GenerateKey(rand.Reader, 2048)
}
