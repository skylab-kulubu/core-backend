package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
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
	if len(os.Args) > 1 && os.Args[1] == ytuBackfillCommandName {
		os.Exit(runYTUBackfill(os.Args[2:], os.Getenv, os.Stdout))
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	// A proxy list that cannot be read stops startup: continuing would either
	// believe a header any caller can write or stop believing the real proxy,
	// and both are silent until someone reads the recorded addresses.
	trustedProxies, err := clientip.RangesFromEnv(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("trusted proxy ranges: %s", trustedProxies)

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
		// Read-only: core holds no manage-clients; Keycloak's operator script creates the roles.
		roleContext, cancelRoleCheck := context.WithTimeout(context.Background(), 15*time.Second)
		missingRoles, err := keycloakDirectory.MissingClientRoles(roleContext, os.Getenv("KEYCLOAK_CLIENT_ID"), identity.CertificateClientRoles)
		cancelRoleCheck()
		if warning := identity.CertificateRolesWarning(os.Getenv("KEYCLOAK_CLIENT_ID"), missingRoles, err); warning != "" {
			log.Print(warning)
		}
		dir = keycloakDirectory
	}
	parse := func(string) (authn.Identity, error) {
		return authn.Identity{}, authn.ErrInvalidToken
	}
	parseSelfDelete := func(string, string) (authn.Identity, error) {
		return authn.Identity{}, authn.ErrInvalidToken
	}
	parseSelfDeleteSudo := func(context.Context, string, string) (authn.Identity, error) {
		return authn.Identity{}, authn.ErrInvalidToken
	}
	// Nil until the sudo path is configured: without it there is no replay.
	var parseSelfDeleteBearer func(string) (authn.Identity, error)
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
		parseSelfDelete = func(accessToken, idToken string) (authn.Identity, error) {
			return authn.ParseSelfDeleteContext(accessToken, idToken, v.Verify, issuer, "account-center", time.Now().UTC(), 5*time.Minute)
		}
		if introspection, ok := sudoIntrospection(os.Getenv, issuer); ok {
			if introspection.ClientID != authn.ResourceAudience {
				log.Printf("account self-delete sudo proof: KEYCLOAK_CLIENT_ID is not %q, so Keycloak will report every sudo token inactive", authn.ResourceAudience)
			}
			parseSelfDeleteSudo = func(ctx context.Context, accessToken, sudoToken string) (authn.Identity, error) {
				ident, err := authn.ParseSelfDeleteSudoContext(ctx, accessToken, sudoToken, v.Verify, introspection, issuer, "account-center", time.Now().UTC())
				if errors.Is(err, authn.ErrIntrospectionUnavailable) {
					// The error names the endpoint and the failure, never a token.
					log.Printf("account self-delete sudo proof: %v", err)
				}
				return ident, err
			}
			// A retry of a key Core already accepted is answered from the
			// stored request with the bearer verified locally: the deletion
			// closes the session the sudo proof is bound to.
			parseSelfDeleteBearer = func(accessToken string) (authn.Identity, error) {
				return authn.ParseSelfDeleteBearer(accessToken, v.Verify, issuer, "account-center", time.Now().UTC())
			}
		} else {
			log.Print("account self-delete sudo proof disabled: KEYCLOAK_CLIENT_ID and KEYCLOAK_CLIENT_SECRET are required")
		}
	}

	gateConfig, err := accessgate.ConfigFromEnv(os.Getenv, issuer)
	if err != nil {
		log.Fatal(err)
	}
	var gate *accessgate.RedisGate
	var accessProjector *account.AccessProjector
	accessMetrics := accessgate.NewMetrics()
	if gateConfig.Mode == accessgate.ModeEnforce {
		redisClient, err := accessgate.NewRedisClient(gateConfig)
		if err != nil {
			log.Fatal(err)
		}
		defer redisClient.Close()
		gate = accessgate.NewRedisGate(
			redisClient, gateConfig.OperationTimeout, gateConfig.RequiredReplicas, gateConfig.WaitTimeout, accessMetrics,
		)
		accessProjector = account.NewAccessProjector(users, gate, nil)
		reconciler := account.NewAccessReconciler(users, gate, nil)
		bootstrapContext, cancelBootstrap := context.WithTimeout(context.Background(), 30*time.Second)
		if err := reconciler.RunOnce(bootstrapContext); err != nil {
			cancelBootstrap()
			log.Fatal(err)
		}
		cancelBootstrap()
		projectionContext, stopProjection := context.WithCancel(context.Background())
		defer stopProjection()
		account.MaintainAccessProjection(projectionContext, accessProjector, reconciler, time.Minute, func(err error) {
			log.Printf("account access projection: %v", err)
		})
	}

	workerEnabled, erasureConfig, err := accountErasureStartup(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	if err := validateAccountErasureGate(workerEnabled, gateConfig.Mode); err != nil {
		log.Fatal(err)
	}
	selfDeletionConfig, err := accountSelfDeletionConfig(os.Getenv, workerEnabled)
	if err != nil {
		log.Fatal(err)
	}
	selfDeletion, err := account.NewSelfDeletion(users, accessProjector, selfDeletionConfig)
	if err != nil {
		log.Fatal(err)
	}
	var erasureGauges *account.ErasureGauges
	if workerEnabled {
		// The watchdog counts open, overdue (ACCOUNT_ERASURE_ALERT_AFTER) and
		// manual-intervention requests every five minutes for /v1/metrics and
		// writes one account_erasure_attention line per request that needs a
		// person. The service erasure steps join the saga in ticket 07.
		erasureGauges = account.NewErasureGauges()
		account.MaintainWatchdog(context.Background(), account.NewWatchdog(users, erasureGauges, account.WatchdogConfig{
			AlertAfter: erasureConfig.AlertAfter,
		}), account.DefaultWatchdogInterval, func(err error) {
			log.Printf("account erasure watchdog: %v", err)
		})
		log.Printf("account erasure watchdog: alert after %s; periodic destruction interval %s",
			erasureConfig.AlertAfter, erasureConfig.PeriodicDestructionInterval)
		account.Maintain(
			context.Background(),
			account.NewWorker(users, identity.NewAccountIdentity(dir), account.WorkerConfig{
				Lease:                5 * time.Minute,
				RetryDelay:           30 * time.Second,
				MaxAttempts:          8,
				StepTimeout:          20 * time.Second,
				DeferredRetryHorizon: uploadStagingConfig.Grace + 24*time.Hour,
				AccessBlocker:        gate,
			}, media.NewImmediateBlobEraser(mediaStore, blobs)),
			2*time.Second,
			func(err error) { log.Printf("account erasure worker: %v", err) },
		)
	} else {
		log.Print("account erasure worker disabled: ACCOUNT_ERASURE_WORKER_ENABLED is not true")
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
			TemplateKey:            templateKey(os.LookupEnv, "SKYMAIL_WELCOME_TEMPLATE_KEY", mail.DefaultWelcomeTemplateKey),
			CertificateTemplateKey: templateKey(os.LookupEnv, "SKYMAIL_CERTIFICATE_TEMPLATE_KEY", mail.DefaultCertificateTemplateKey),
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
		// Said once, at startup: a kind with neither a key nor an id drops every
		// mail of that kind, and the send path stays silent by design.
		sky.WarnUnconfiguredTemplates()
		if sky.Configured() {
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
			AccessProjector:       accessProjector,
		}, mailer),
		Events:      event.NewService(events, az, cdnBase),
		Seasons:     season.NewService(seasons, az),
		Tickets:     ticketSvc,
		Competitors: competitor.NewService(competitors, events, az),
		Media: media.NewServiceWithOptions(mediaStore, blobs, az, cdnBase, media.ServiceOptions{
			UploadStagingGrace: uploadStagingConfig.Grace,
		}),
		URLs:                   shorturl.NewService(urlStore, az),
		Certificates:           certSvc,
		SkyPass:                skypass.NewService(users, az, skypass.NewSigner(passKey, skypass.DefaultTTL)),
		Mail:                   mailer,
		EventMail:              eventmail.New(events, tickets, users, lists, az, mailSnapshots),
		ParseToken:             parse,
		AccountAccessGate:      optionalAccountAccessGate(gate),
		AccountAccessMetrics:   accessMetrics,
		AccountErasureMetrics:  optionalErasureMetrics(erasureGauges),
		SelfDeletion:           selfDeletion,
		ParseSelfDeleteContext: parseSelfDelete,
		ParseSelfDeleteSudo:    parseSelfDeleteSudo,
		ParseSelfDeleteBearer:  parseSelfDeleteBearer,
		URLAttributionGuard: func(ctx context.Context, id uuid.UUID) (user.AttributionState, error) {
			return users.AttributionState(ctx, id)
		},
		TrustedProxies: trustedProxies,
	})

	addr := os.Getenv("PORT")
	if addr == "" {
		addr = "8080"
	}
	if err := app.Listen(":" + addr); err != nil {
		log.Fatal(err)
	}
}

// sudoIntrospection builds the client Core uses to ask the realm about a
// sky-account Sudo mode token: the issuer's RFC 7662 endpoint and Core's own
// confidential client, the same KEYCLOAK_CLIENT_ID/KEYCLOAK_CLIENT_SECRET pair
// Core already uses for its client-credentials token. It reports false when a
// part is missing; the intake then refuses every sudo proof.
func sudoIntrospection(getenv func(string) string, issuer string) (authn.Introspection, bool) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	clientID := getenv("KEYCLOAK_CLIENT_ID")
	clientSecret := getenv("KEYCLOAK_CLIENT_SECRET")
	if issuer == "" || strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return authn.Introspection{}, false
	}
	return authn.Introspection{
		URL:          issuer + "/protocol/openid-connect/token/introspect",
		ClientID:     clientID,
		ClientSecret: clientSecret,
		HTTP:         &http.Client{Timeout: authn.DefaultIntrospectionTimeout},
		Timeout:      authn.DefaultIntrospectionTimeout,
	}, true
}

func optionalAccountAccessGate(gate *accessgate.RedisGate) accessgate.Reader {
	if gate == nil {
		return nil
	}
	return gate
}

func accountSelfDeletionConfig(getenv func(string) string, enabled bool) (account.SelfDeletionConfig, error) {
	config := account.SelfDeletionConfig{Enabled: enabled, ReceiptTTL: 90 * 24 * time.Hour}
	if !enabled {
		return config, nil
	}
	raw := strings.TrimSpace(getenv("ACCOUNT_DELETION_RECEIPT_KEY"))
	key, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return account.SelfDeletionConfig{}, errors.New("ACCOUNT_DELETION_RECEIPT_KEY must be an unpadded base64url-encoded 32-byte key when account erasure is enabled")
	}
	config.ReceiptKey = key
	return config, nil
}

// accountErasureStartup reads the erasure master flag and, when it is on, the
// Erasure command configuration. Every error names a variable, never a value.
func accountErasureStartup(getenv func(string) string) (bool, erasure.Config, error) {
	enabled, err := accountErasureWorkerEnabled(getenv)
	if err != nil {
		return false, erasure.Config{}, err
	}
	config, err := erasure.ConfigFromEnv(getenv, enabled)
	if err != nil {
		return false, erasure.Config{}, err
	}
	return enabled, config, nil
}

// optionalErasureMetrics keeps a worker that is off from publishing gauges.
func optionalErasureMetrics(gauges *account.ErasureGauges) interface{ Prometheus() string } {
	if gauges == nil {
		return nil
	}
	return gauges
}

func validateAccountErasureGate(workerEnabled bool, mode accessgate.Mode) error {
	if workerEnabled && mode != accessgate.ModeEnforce {
		return errors.New("ACCOUNT_ACCESS_GATE_MODE=enforce is required when account erasure is enabled")
	}
	return nil
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

// templateKey reads a SkyMail template key. An unset variable takes the seeded
// default, because addressing by key is what core wants everywhere. A variable
// set to an empty value opts that kind back onto its template id, which is how
// the key rollout is held back or rolled back without a code change.
func templateKey(lookup func(string) (string, bool), name, fallback string) string {
	raw, ok := lookup(name)
	if !ok {
		return fallback
	}
	return strings.TrimSpace(raw)
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
