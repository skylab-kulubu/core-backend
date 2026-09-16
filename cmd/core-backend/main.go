package main

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
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

	az := authz.NewAuthorizer(authz.DefaultPolicy())
	users := user.NewPostgresStore(pool)
	events := event.NewPostgresStore(pool)
	seasons := season.NewPostgresStore(pool)
	tickets := ticket.NewPostgresStore(pool)
	competitors := competitor.NewPostgresStore(pool)
	mediaStore := media.NewPostgresStore(pool)
	var blobs media.BlobStore = media.NewMemoryBlob()
	if os.Getenv("R2_ENDPOINT") != "" {
		blobs = media.NewR2(media.R2Config{
			Endpoint:  os.Getenv("R2_ENDPOINT"),
			AccessKey: os.Getenv("R2_ACCESS_KEY"),
			SecretKey: os.Getenv("R2_SECRET_KEY"),
			Bucket:    os.Getenv("R2_BUCKET"),
		})
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
	if jwksURL != "" && issuer != "" {
		v := authn.NewJWKS(jwksURL)
		parse = func(token string) (authn.Identity, error) {
			return authn.ParseAndVerify(token, v.Verify, issuer, authn.ResourceAudience)
		}
	}

	var mailer mail.Mailer
	if os.Getenv("SKYMAIL_URL") != "" && os.Getenv("SKYMAIL_WELCOME_TEMPLATE_ID") != "" {
		tid, err := uuid.Parse(os.Getenv("SKYMAIL_WELCOME_TEMPLATE_ID"))
		if err != nil {
			log.Fatal("SKYMAIL_WELCOME_TEMPLATE_ID must be a UUID")
		}
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
		mailer = &mail.SkyMail{
			BaseURL:    os.Getenv("SKYMAIL_URL"),
			TemplateID: tid,
			Tokens: mail.ClientCredentials{
				TokenURL:     kc + "/realms/" + realm + "/protocol/openid-connect/token",
				ClientID:     os.Getenv("KEYCLOAK_CLIENT_ID"),
				ClientSecret: os.Getenv("KEYCLOAK_CLIENT_SECRET"),
			},
		}
	}

	app := httpx.New(httpx.Deps{
		Users:       user.NewService(users, dir),
		Identity:    identity.NewService(dir, users, az, mailer),
		Events:      event.NewService(events, az),
		Seasons:     season.NewService(seasons, az),
		Tickets:     ticket.NewService(tickets, events, az, users),
		Competitors: competitor.NewService(competitors, events, az),
		Media:       media.NewService(mediaStore, blobs, az, os.Getenv("CDN_BASE")),
		URLs:        shorturl.NewService(shorturl.NewPostgresStore(pool), az),
		Mail:        mailer,
		ParseToken:  parse,
	})

	addr := os.Getenv("PORT")
	if addr == "" {
		addr = "8080"
	}
	if err := app.Listen(":" + addr); err != nil {
		log.Fatal(err)
	}
}
