package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	deletedSubject = "00000000-0000-4000-8000-000000000000"
	contractKey    = "skylab:account-access:v1:contract"
	contractValue  = `sha256(iss\0sub);marker=1;ttl=none`
	markerPrefix   = "skylab:account-access:v1:blocked:"
	maxBody        = 4096
	maxEmails      = 3
	maxEmailLength = 254
)

type decision int

const (
	allowed decision = iota
	blocked
	unavailable
)

// gate reads the shared account-access marker with the reader ACL (GET/MGET only).
type gate struct {
	client *redis.Client
	issuer string
}

func (g gate) check(ctx context.Context, subject string) decision {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	digest := sha256.Sum256([]byte(g.issuer + "\x00" + subject))
	values, err := g.client.MGet(ctx, contractKey, markerPrefix+hex.EncodeToString(digest[:])).Result()
	if err != nil || len(values) != 2 {
		return unavailable
	}
	if contract, ok := values[0].(string); !ok || contract != contractValue {
		return unavailable
	}
	switch marker := values[1].(type) {
	case nil:
		return allowed
	case string:
		if marker == "1" {
			return blocked
		}
	}
	return unavailable
}

type formsServer struct {
	db       *pgxpool.Pool
	tokens   *verifier
	gate     gate
	resource string
	role     string
	azp      string

	// Harness controls (a separate listener, never core's): a hold after the commit and a
	// forced status, both for the next erase calls only.
	holdSeconds atomic.Int64
	failStatus  atomic.Int64
	failCount   atomic.Int64
	mu          sync.Mutex
	puts        map[string]int
}

func runForms() {
	getenv := func(key string) string {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			log.Fatalf("%s is required", key)
		}
		return value
	}
	issuer := getenv("FORMS_ISSUER")
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, getenv("FORMS_DATABASE_URL"))
	if err != nil {
		log.Fatal("database configuration invalid")
	}
	for attempt := 0; ; attempt++ {
		if _, err = pool.Exec(ctx, schema); err == nil {
			break
		}
		if attempt > 30 {
			log.Fatalf("schema: %v", err)
		}
		time.Sleep(time.Second)
	}
	certificate, err := tls.LoadX509KeyPair(getenv("ACCOUNT_ACCESS_REDIS_TLS_CERT_FILE"), getenv("ACCOUNT_ACCESS_REDIS_TLS_KEY_FILE"))
	if err != nil {
		log.Fatal("account access client certificate unreadable")
	}
	caPEM, err := os.ReadFile(getenv("ACCOUNT_ACCESS_REDIS_CA_CERT_FILE"))
	if err != nil {
		log.Fatal("account access CA unreadable")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	db, _ := strconv.Atoi(getenv("ACCOUNT_ACCESS_REDIS_DB"))
	redisClient := redis.NewClient(&redis.Options{
		Addr:     getenv("ACCOUNT_ACCESS_REDIS_ADDR"),
		Username: getenv("ACCOUNT_ACCESS_REDIS_USERNAME"),
		Password: getenv("ACCOUNT_ACCESS_REDIS_PASSWORD"),
		DB:       db,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			ServerName:   getenv("ACCOUNT_ACCESS_REDIS_TLS_SERVER_NAME"),
			Certificates: []tls.Certificate{certificate},
			RootCAs:      roots,
		},
		MaxRetries: -1,
	})
	jwks := os.Getenv("FORMS_JWKS_URL")
	if jwks == "" {
		jwks = issuer + "/protocol/openid-connect/certs"
	}
	s := &formsServer{
		db:       pool,
		tokens:   newVerifier(issuer, jwks),
		gate:     gate{client: redisClient, issuer: issuer},
		resource: "forms",
		role:     "skyforms:account:erase",
		azp:      "core-erasure",
		puts:     map[string]int{},
	}

	api := http.NewServeMux()
	api.HandleFunc("PUT /internal/v1/account-erasures/{request_id}", s.erase)
	api.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	api.HandleFunc("GET /api/me/responses", s.myResponses)

	control := http.NewServeMux()
	control.HandleFunc("POST /harness/hold", func(w http.ResponseWriter, r *http.Request) {
		seconds, _ := strconv.ParseInt(r.URL.Query().Get("seconds"), 10, 64)
		s.holdSeconds.Store(seconds)
		w.WriteHeader(http.StatusNoContent)
	})
	control.HandleFunc("POST /harness/fail", func(w http.ResponseWriter, r *http.Request) {
		status, _ := strconv.ParseInt(r.URL.Query().Get("status"), 10, 64)
		count, _ := strconv.ParseInt(r.URL.Query().Get("count"), 10, 64)
		s.failStatus.Store(status)
		s.failCount.Store(count)
		w.WriteHeader(http.StatusNoContent)
	})
	control.HandleFunc("GET /harness/stats/{request_id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("request_id")
		s.mu.Lock()
		puts := s.puts[id]
		s.mu.Unlock()
		var runs int
		if err := s.db.QueryRow(r.Context(), `SELECT count(*) FROM harness_erase_runs WHERE request_id::text = $1`, id).Scan(&runs); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"puts": puts, "erase_runs": runs})
	})
	go func() {
		log.Fatal(http.ListenAndServe(":9090", accessLog(control)))
	}()
	log.Print("forms stub listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", accessLog(api)))
}

const schema = `
CREATE TABLE IF NOT EXISTS account_erasure_receipts (
  request_id uuid PRIMARY KEY,
  completed_at timestamptz NOT NULL,
  counts jsonb NOT NULL
);
CREATE TABLE IF NOT EXISTS forms (
  id uuid PRIMARY KEY,
  title text NOT NULL,
  owned_by text NOT NULL
);
CREATE TABLE IF NOT EXISTS responses (
  id uuid PRIMARY KEY,
  form_id uuid NOT NULL REFERENCES forms (id),
  user_id text NULL,
  data jsonb NOT NULL,
  review_note text NULL,
  reviewed_by text NULL,
  status text NOT NULL DEFAULT 'submitted',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS collaborators (
  form_id uuid NOT NULL REFERENCES forms (id),
  user_id text NOT NULL,
  role text NOT NULL,
  PRIMARY KEY (form_id, user_id)
);
-- Harness evidence only: one row per erasure the stub actually ran, written in the same
-- transaction as the receipt. It holds no subject and no address.
CREATE TABLE IF NOT EXISTS harness_erase_runs (
  request_id uuid NOT NULL,
  ran_at timestamptz NOT NULL DEFAULT now()
);
`

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// accessLog writes the method, the path, the status and the duration: never a header or body.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, recorder.status, time.Since(started).Round(time.Millisecond))
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func problem(w http.ResponseWriter, status int, code, title string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": title, "status": status, "code": code})
}

func unavailableProblem(w http.ResponseWriter, code string, retryAfter int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	problem(w, http.StatusServiceUnavailable, code, "Erasure temporarily unavailable")
}

// myResponses is an ordinary Forms read: any token with aud forms, the caller's own marker
// checked first. It answers a count, never the answers themselves.
func (s *formsServer) myResponses(w http.ResponseWriter, r *http.Request) {
	c, err := s.tokens.verify(r.Context(), r.Header.Get("Authorization"))
	if errors.Is(err, errKeysUnavailable) {
		unavailableProblem(w, "token_keys_unavailable", 1)
		return
	}
	if err != nil || !contains(c.audiences(), s.resource) {
		problem(w, http.StatusUnauthorized, "unauthorized", "Unauthorized")
		return
	}
	switch s.gate.check(r.Context(), c.Subject) {
	case blocked:
		problem(w, http.StatusUnauthorized, "unauthorized", "Unauthorized")
		return
	case unavailable:
		unavailableProblem(w, "access_gate_unavailable", 1)
		return
	}
	var n int
	if err := s.db.QueryRow(r.Context(), `SELECT count(*) FROM responses WHERE user_id = $1`, c.Subject).Scan(&n); err != nil {
		unavailableProblem(w, "store_unavailable", 1)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"responses": n})
}

type receipt struct {
	CompletedAt time.Time
	Counts      map[string]int64
}

func completed(w http.ResponseWriter, requestID uuid.UUID, rc receipt) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"request_id":   requestID.String(),
		"status":       "completed",
		"completed_at": rc.CompletedAt.UTC().Format(time.RFC3339Nano),
		"counts":       rc.Counts,
	})
}

func (s *formsServer) erase(w http.ResponseWriter, r *http.Request) {
	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "X-Real-Ip"} {
		if r.Header.Get(header) != "" {
			http.NotFound(w, r)
			return
		}
	}
	rawID := r.PathValue("request_id")
	s.mu.Lock()
	s.puts[rawID]++
	s.mu.Unlock()
	if status := s.failStatus.Load(); status != 0 && s.failCount.Add(-1) >= 0 {
		if status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "30")
		}
		problem(w, int(status), "harness_injected", "Injected by the harness")
		return
	}

	c, err := s.tokens.verify(r.Context(), r.Header.Get("Authorization"))
	switch {
	case errors.Is(err, errKeysUnavailable):
		unavailableProblem(w, "token_keys_unavailable", 30)
		return
	case err != nil:
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		problem(w, http.StatusUnauthorized, "erasure_unauthorized", "Erasure token rejected")
		return
	}
	resource, hasResource := c.ResourceAccess[s.resource]
	if c.AuthorizedParty != s.azp || !contains(c.audiences(), s.resource) || !hasResource || !contains(resource.Roles, s.role) {
		problem(w, http.StatusForbidden, "erasure_forbidden", "Erasure forbidden")
		return
	}
	switch s.gate.check(r.Context(), c.Subject) {
	case blocked:
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		problem(w, http.StatusUnauthorized, "erasure_unauthorized", "Erasure token rejected")
		return
	case unavailable:
		unavailableProblem(w, "access_gate_unavailable", 30)
		return
	}

	requestID, errPath := uuid.Parse(rawID)
	cmd, ok := parseCommand(r)
	if errPath != nil || requestID.String() != rawID || !ok || cmd.requestID != requestID {
		problem(w, http.StatusBadRequest, "invalid_erasure_command", "Invalid erasure command")
		return
	}
	if rc, found, err := s.findReceipt(r.Context(), s.db, requestID); err != nil {
		unavailableProblem(w, "erasure_store_unavailable", 30)
		return
	} else if found {
		completed(w, requestID, rc)
		return
	}
	switch s.gate.check(r.Context(), cmd.subjectID) {
	case allowed:
		problem(w, http.StatusConflict, "subject_not_blocked", "Subject is not blocked")
		return
	case unavailable:
		unavailableProblem(w, "subject_block_unverifiable", 30)
		return
	}

	rc, err := s.eraseInTransaction(r.Context(), requestID, cmd)
	if err != nil {
		log.Printf("account erasure failed request_id=%s code=erasure_store_unavailable", requestID)
		unavailableProblem(w, "erasure_store_unavailable", 30)
		return
	}
	log.Printf("account erasure completed request_id=%s", requestID)
	if hold := s.holdSeconds.Load(); hold > 0 {
		log.Printf("harness hold request_id=%s seconds=%d", requestID, hold)
		time.Sleep(time.Duration(hold) * time.Second)
	}
	completed(w, requestID, rc)
}

type command struct {
	requestID uuid.UUID
	subjectID string
	emails    []string
}

// parseCommand reads exactly request_id, subject_id and emails from a JSON object of at most
// 4 KB; anything else is a 400.
func parseCommand(r *http.Request) (command, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return command{}, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return command{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var raw struct {
		RequestID *string   `json:"request_id"`
		SubjectID *string   `json:"subject_id"`
		Emails    *[]string `json:"emails"`
	}
	if err := decoder.Decode(&raw); err != nil || decoder.More() {
		return command{}, false
	}
	if raw.RequestID == nil || raw.SubjectID == nil || raw.Emails == nil {
		return command{}, false
	}
	requestID, err := uuid.Parse(*raw.RequestID)
	if err != nil || requestID.String() != *raw.RequestID {
		return command{}, false
	}
	subject, err := uuid.Parse(*raw.SubjectID)
	if err != nil || subject.String() != *raw.SubjectID || subject.String() == deletedSubject {
		return command{}, false
	}
	if len(*raw.Emails) > maxEmails {
		return command{}, false
	}
	emails := make([]string, 0, len(*raw.Emails))
	for _, email := range *raw.Emails {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" || len(email) > maxEmailLength || !strings.Contains(email, "@") {
			return command{}, false
		}
		emails = append(emails, email)
	}
	return command{requestID: requestID, subjectID: subject.String(), emails: emails}, true
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *formsServer) findReceipt(ctx context.Context, q querier, requestID uuid.UUID) (receipt, bool, error) {
	var rc receipt
	var counts []byte
	err := q.QueryRow(ctx, `SELECT completed_at, counts FROM account_erasure_receipts WHERE request_id = $1`, requestID).Scan(&rc.CompletedAt, &counts)
	if errors.Is(err, pgx.ErrNoRows) {
		return receipt{}, false, nil
	}
	if err != nil {
		return receipt{}, false, err
	}
	if err := json.Unmarshal(counts, &rc.Counts); err != nil {
		return receipt{}, false, err
	}
	return rc, true, nil
}

// eraseInTransaction applies spec §3.3's proposal to the stub's tables and writes the receipt in
// the same transaction, under an advisory lock on the request id.
func (s *formsServer) eraseInTransaction(ctx context.Context, requestID uuid.UUID, cmd command) (receipt, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return receipt{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 7351))`, requestID.String()); err != nil {
		return receipt{}, err
	}
	if rc, found, err := s.findReceipt(ctx, tx, requestID); err != nil || found {
		return rc, err
	}
	counts := map[string]int64{}
	exec := func(key, sql string, args ...any) error {
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return err
		}
		counts[key] += tag.RowsAffected()
		return nil
	}
	steps := []struct {
		key  string
		sql  string
		args []any
	}{
		{"responses_redacted", `UPDATE responses SET user_id = NULL, data = '{}'::jsonb, review_note = NULL WHERE user_id = $1`, []any{cmd.subjectID}},
		{"guest_responses_redacted", `UPDATE responses SET data = '{}'::jsonb, review_note = NULL
		   WHERE user_id IS NULL AND cardinality($1::text[]) > 0
		     AND EXISTS (SELECT 1 FROM jsonb_each_text(data) answer WHERE lower(btrim(answer.value)) = ANY ($1::text[]))`, []any{cmd.emails}},
		{"actor_columns_replaced", `UPDATE responses SET reviewed_by = $2 WHERE reviewed_by = $1`, []any{cmd.subjectID, deletedSubject}},
		{"collaborators_deleted", `DELETE FROM collaborators WHERE user_id = $1 AND role <> 'Owner'`, []any{cmd.subjectID}},
		{"actor_columns_replaced", `UPDATE collaborators SET user_id = $2 WHERE user_id = $1 AND role = 'Owner'`, []any{cmd.subjectID, deletedSubject}},
		{"actor_columns_replaced", `UPDATE forms SET owned_by = $2 WHERE owned_by = $1`, []any{cmd.subjectID, deletedSubject}},
	}
	for _, step := range steps {
		if err := exec(step.key, step.sql, step.args...); err != nil {
			return receipt{}, err
		}
	}
	rc := receipt{CompletedAt: time.Now().UTC(), Counts: counts}
	encoded, _ := json.Marshal(counts)
	if _, err := tx.Exec(ctx, `INSERT INTO account_erasure_receipts (request_id, completed_at, counts) VALUES ($1, $2, $3)`, requestID, rc.CompletedAt, encoded); err != nil {
		return receipt{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO harness_erase_runs (request_id) VALUES ($1)`, requestID); err != nil {
		return receipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return receipt{}, err
	}
	// Postgres keeps microseconds; answer what a replay will read back.
	rc.CompletedAt = rc.CompletedAt.Truncate(time.Microsecond)
	return rc, nil
}
