package main

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
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

	parse := authn.ParseAccessToken
	jwksURL := os.Getenv("KEYCLOAK_JWKS_URL")
	if jwksURL == "" {
		base := strings.TrimRight(os.Getenv("KEYCLOAK_URL"), "/")
		realm := os.Getenv("KEYCLOAK_REALM")
		if parts := strings.SplitN(base, "/realms/", 2); len(parts) == 2 {
			base = parts[0]
			if realm == "" {
				realm = parts[1]
			}
		}
		if base != "" && realm != "" {
			jwksURL = base + "/realms/" + realm + "/protocol/openid-connect/certs"
		}
	}
	if jwksURL != "" {
		v := authn.NewJWKS(jwksURL)
		parse = func(token string) (authn.Identity, error) {
			return authn.ParseAndVerify(token, v.Verify)
		}
	}

	app := httpx.New(httpx.Deps{
		Users:       user.NewService(users),
		Identity:    identity.NewService(dir, users, az),
		Events:      event.NewService(events, az),
		Seasons:     season.NewService(seasons, az),
		Tickets:     ticket.NewService(tickets, events, az),
		Competitors: competitor.NewService(competitors, events, az),
		Media:       media.NewService(mediaStore, blobs, az, os.Getenv("CDN_BASE")),
		URLs:        shorturl.NewService(shorturl.NewPostgresStore(pool), az),
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
