package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventmail"
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
	mediaPurgeConfig, err := media.BlobPurgeConfigFromEnv(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	uploadStagingConfig, err := media.UploadStagingConfigFromEnv(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	mediaPurgeContext, stopMediaPurge := context.WithCancel(context.Background())
	defer stopMediaPurge()
	media.MaintainBlobPurge(mediaPurgeContext, mediaStore, blobs, mediaPurgeConfig, func(err error) {
		log.Printf("media blob purge: %v", err)
	})
	media.MaintainUploadStaging(mediaPurgeContext, mediaStore, blobs, uploadStagingConfig, func(err error) {
		log.Printf("media upload staging cleanup: %v", err)
	})
	media.MaintainCoverColorBackfill(context.Background(), mediaStore, blobs, time.Minute, func(err error) {
		log.Printf("media cover color backfill: %v", err)
	})

	dir := identity.Directory(identity.NewMemory())
	keycloakConfigured := strings.TrimSpace(os.Getenv("KEYCLOAK_URL")) != ""
	if keycloakConfigured {
		if os.Getenv("KEYCLOAK_REALM") == "" || os.Getenv("KEYCLOAK_CLIENT_ID") == "" || os.Getenv("KEYCLOAK_CLIENT_SECRET") == "" {
			log.Fatal("KEYCLOAK_REALM, KEYCLOAK_CLIENT_ID, and KEYCLOAK_CLIENT_SECRET are required with KEYCLOAK_URL")
		}
		keycloakDirectory := identity.NewKeycloak(identity.KeycloakConfig{
			URL:          os.Getenv("KEYCLOAK_URL"),
			Realm:        os.Getenv("KEYCLOAK_REALM"),
			ClientID:     os.Getenv("KEYCLOAK_CLIENT_ID"),
			ClientSecret: os.Getenv("KEYCLOAK_CLIENT_SECRET"),
		})
		roleContext, cancelRoleSetup := context.WithTimeout(context.Background(), 15*time.Second)
		if err := keycloakDirectory.EnsureClientRoles(roleContext, os.Getenv("KEYCLOAK_CLIENT_ID"), []string{
			"certificate:template:manage",
			"certificate:binding:manage",
			"certificate:issue",
			"certificate:revoke",
		}); err != nil {
			log.Printf("certificate client roles could not be ensured: %v", err)
		}
		cancelRoleSetup()
		dir = keycloakDirectory
	}
	workerEnabled, err := accountErasureWorkerEnabled(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	if workerEnabled {
		account.Maintain(
			context.Background(),
			account.NewWorker(users, identity.NewAccountIdentity(dir), account.WorkerConfig{
				Lease:                5 * time.Minute,
				RetryDelay:           30 * time.Second,
				MaxAttempts:          8,
				StepTimeout:          20 * time.Second,
				DeferredRetryHorizon: uploadStagingConfig.Grace + 24*time.Hour,
			}, media.NewImmediateBlobEraser(mediaStore, blobs)),
			2*time.Second,
			func(err error) { log.Printf("account erasure worker: %v", err) },
		)
	} else {
		log.Print("account erasure worker disabled: ACCOUNT_ERASURE_WORKER_ENABLED is not true")
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
	if base != "" && realm != "" && os.Getenv("KEYCLOAK_CLIENT_ID") != "" && os.Getenv("KEYCLOAK_CLIENT_SECRET") != "" {
		sky = &mail.SkyMail{
			BaseURL: mail.APIOrigin(os.Getenv("SKYMAIL_URL")),
			HTTP:    &http.Client{Timeout: 15 * time.Second},
			Tokens: mail.ClientCredentials{
				TokenURL:     base + "/realms/" + realm + "/protocol/openid-connect/token",
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
		if sky.TemplateID != uuid.Nil || sky.CertificateTemplateID != uuid.Nil {
			mailer = sky
		}
	}

	var render certificate.Renderer
	if baseURL := gotenbergURL(); baseURL != "" {
		render = &certificate.Gotenberg{BaseURL: baseURL}
	}

	passKey, err := loadSkyPassKey()
	if err != nil {
		log.Fatal(err)
	}

	ticketSvc := ticket.NewService(tickets, events, az, users, dir)
	certSvc := certificate.NewServiceWithOptions(certs, tickets, events, users, az, render, sky, certificate.Options{
		PublicAPIOrigin: os.Getenv("PUBLIC_API_ORIGIN"),
		VerifyOrigin:    os.Getenv("PUBLIC_VERIFY_ORIGIN"),
		Templates:       certs,
		Jobs:            certs,
		Artifacts:       blobs,
		Assets:          certificate.MediaAssets{Media: mediaStore, Blobs: blobs},
	})
	certificate.MaintainIssuance(context.Background(), certSvc, 2*time.Second, 10, func(err error) {
		log.Printf("certificate issuance worker: %v", err)
	})
	var lists mail.Lists
	mailSnapshots := eventmail.NewPostgresSnapshotStore(pool)
	if sky != nil {
		lists = sky
		eventmail.MaintainSnapshotRetention(context.Background(), mailSnapshots, sky, time.Hour, func(err error) {
			log.Printf("event mail snapshot retention: %v", err)
		})
	}

	urlStore := shorturl.NewPostgresStore(pool)
	shorturl.MaintainHitRetention(context.Background(), urlStore, time.Hour, func(err error) {
		log.Printf("short-link hit retention: %v", err)
	})

	app := httpx.New(httpx.Deps{
		Users: user.NewService(users, dir),
		Identity: identity.NewServiceWithOptions(dir, users, az, identity.Options{
			AccountErasureEnabled: workerEnabled,
		}, mailer),
		Events:      event.NewService(events, az, cdnBase),
		Seasons:     season.NewService(seasons, az),
		Tickets:     ticketSvc,
		Competitors: competitor.NewService(competitors, events, az),
		Media: media.NewServiceWithOptions(mediaStore, blobs, az, cdnBase, media.ServiceOptions{
			UploadStagingGrace: uploadStagingConfig.Grace,
		}),
		URLs:         shorturl.NewService(urlStore, az),
		Certificates: certSvc,
		SkyPass:      skypass.NewService(users, az, skypass.NewSigner(passKey, skypass.DefaultTTL)),
		Mail:         mailer,
		EventMail:    eventmail.New(events, tickets, users, lists, az, mailSnapshots),
		ParseToken:   parse,
		URLAttributionGuard: func(ctx context.Context, id uuid.UUID) bool {
			allowed, err := users.CanAttribute(ctx, id)
			return err == nil && allowed
		},
	})

	addr := os.Getenv("PORT")
	if addr == "" {
		addr = "8080"
	}
	if err := app.Listen(":" + addr); err != nil {
		log.Fatal(err)
	}
}

func accountErasureWorkerEnabled(getenv func(string) string) (bool, error) {
	raw := strings.TrimSpace(getenv("ACCOUNT_ERASURE_WORKER_ENABLED"))
	switch raw {
	case "", "false":
		return false, nil
	case "true":
		for _, key := range []string{"KEYCLOAK_URL", "KEYCLOAK_REALM", "KEYCLOAK_CLIENT_ID", "KEYCLOAK_CLIENT_SECRET"} {
			if strings.TrimSpace(getenv(key)) == "" {
				return false, fmt.Errorf("%s is required when ACCOUNT_ERASURE_WORKER_ENABLED=true", key)
			}
		}
		return true, nil
	default:
		return false, fmt.Errorf("ACCOUNT_ERASURE_WORKER_ENABLED must be true or false")
	}
}

func gotenbergURL() string {
	if explicit := strings.TrimSpace(os.Getenv("GOTENBERG_URL")); explicit != "" {
		return explicit
	}
	return "http://gotenberg:3000"
}

func loadSkyPassKey() (*ecdsa.PrivateKey, error) {
	if raw := os.Getenv("SKYPASS_EC_PRIVATE_KEY"); raw != "" {
		return skypass.ParseSigningKey([]byte(raw))
	}
	if raw := os.Getenv("SKYPASS_RSA_PRIVATE_KEY"); raw != "" {
		return skypass.ParseSigningKey([]byte(raw))
	}
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}
