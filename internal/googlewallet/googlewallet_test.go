package googlewallet

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testAccount is a service account key made for the test: the same JSON
// shape Google's console downloads, with a fresh RSA key.
func testAccount(t *testing.T, tokenURI string) (string, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	raw, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "skylab-wallet-test",
		"private_key_id": "kid-1",
		"private_key":    pemKey,
		"client_email":   "wallet@skylab-wallet-test.iam.gserviceaccount.com",
		"client_id":      "1234567890",
		"token_uri":      tokenURI,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), key
}

func TestParseServiceAccountTakesJSONOrBase64(t *testing.T) {
	t.Parallel()
	raw, _ := testAccount(t, "https://oauth2.googleapis.com/token")
	for name, value := range map[string]string{
		"json":            raw,
		"json, spaced":    "  " + raw + "\n",
		"base64":          base64.StdEncoding.EncodeToString([]byte(raw)),
		"base64, raw url": base64.RawURLEncoding.EncodeToString([]byte(raw)),
	} {
		account, err := ParseServiceAccount(value)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if account.ClientEmail != "wallet@skylab-wallet-test.iam.gserviceaccount.com" || account.PrivateKeyID != "kid-1" {
			t.Fatalf("%s: account %q %q", name, account.ClientEmail, account.PrivateKeyID)
		}
		if account.TokenURI != "https://oauth2.googleapis.com/token" {
			t.Fatalf("%s: token uri %q", name, account.TokenURI)
		}
	}
}

func TestParseServiceAccountRefusesWithoutEchoingTheValue(t *testing.T) {
	t.Parallel()
	raw, _ := testAccount(t, "https://oauth2.googleapis.com/token")
	var fields map[string]string
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	broken := func(change func(map[string]string)) string {
		copied := map[string]string{}
		for k, v := range fields {
			copied[k] = v
		}
		change(copied)
		out, _ := json.Marshal(copied)
		return string(out)
	}
	cases := map[string]string{
		"empty":      "",
		"not json":   "not-a-key",
		"wrong type": broken(func(m map[string]string) { m["type"] = "authorized_user" }),
		"no email":   broken(func(m map[string]string) { delete(m, "client_email") }),
		"no key":     broken(func(m map[string]string) { delete(m, "private_key") }),
		"bad key": broken(func(m map[string]string) {
			m["private_key"] = "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"
		}),
		"http token":  broken(func(m map[string]string) { m["token_uri"] = "http://oauth2.googleapis.com/token" }),
		"bad token":   broken(func(m map[string]string) { m["token_uri"] = "::" }),
		"key in json": `{"type":"service_account","client_email":"a@b","private_key":"SECRET-MATERIAL"}`,
	}
	for name, value := range cases {
		_, err := ParseServiceAccount(value)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), "SECRET-MATERIAL") || strings.Contains(err.Error(), "BEGIN") {
			t.Fatalf("%s: error echoes the key: %v", name, err)
		}
	}
}

func TestParseServiceAccountDefaultsTheTokenURI(t *testing.T) {
	t.Parallel()
	raw, _ := testAccount(t, "")
	account, err := ParseServiceAccount(raw)
	if err != nil {
		t.Fatal(err)
	}
	if account.TokenURI != DefaultTokenURL {
		t.Fatalf("token uri %q", account.TokenURI)
	}
}

func TestSaveURLIsASignedSaveToWalletJWTThatNamesTheObjectOnly(t *testing.T) {
	t.Parallel()
	raw, key := testAccount(t, "https://oauth2.googleapis.com/token")
	account, err := ParseServiceAccount(raw)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	client := NewClient(account, Options{Now: func() time.Time { return now }})
	link, err := client.SaveURL([]string{"https://hesap.yildizskylab.com"}, []ObjectRef{{
		ID: "3388000000022222222.sp-ABCDEFGHIJKLMNOPQRSTUVWXYZ", ClassID: "3388000000022222222.skypass-sandbox",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, SaveURLPrefix) {
		t.Fatalf("link %q", link)
	}
	signed := strings.TrimPrefix(link, SaveURLPrefix)
	if len(signed) > MaxSaveJWTLength {
		t.Fatalf("jwt is %d characters, over Google's safe length", len(signed))
	}
	claims := jwt.MapClaims{}
	parsed, err := jwt.NewParser(jwt.WithValidMethods([]string{"RS256"})).ParseWithClaims(signed, claims, func(*jwt.Token) (any, error) {
		return &key.PublicKey, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Header["kid"] != "kid-1" {
		t.Fatalf("kid %v", parsed.Header["kid"])
	}
	if claims["iss"] != account.ClientEmail || claims["aud"] != "google" || claims["typ"] != "savetowallet" {
		t.Fatalf("claims %v", claims)
	}
	if iat, _ := claims["iat"].(float64); int64(iat) != now.Unix() {
		t.Fatalf("iat %v", claims["iat"])
	}
	origins, _ := claims["origins"].([]any)
	if len(origins) != 1 || origins[0] != "https://hesap.yildizskylab.com" {
		t.Fatalf("origins %v", claims["origins"])
	}
	payload, _ := claims["payload"].(map[string]any)
	objects, _ := payload["genericObjects"].([]any)
	if len(objects) != 1 {
		t.Fatalf("payload %v", payload)
	}
	object, _ := objects[0].(map[string]any)
	if len(object) != 2 || object["id"] != "3388000000022222222.sp-ABCDEFGHIJKLMNOPQRSTUVWXYZ" || object["classId"] != "3388000000022222222.skypass-sandbox" {
		// Only the reference: the TOTP key stays in the object core
		// inserted, never in a link (Google's rotating barcode guidance).
		t.Fatalf("object %v", object)
	}
}

func TestSaveURLSendsAnEmptyOriginsList(t *testing.T) {
	t.Parallel()
	raw, key := testAccount(t, "https://oauth2.googleapis.com/token")
	account, err := ParseServiceAccount(raw)
	if err != nil {
		t.Fatal(err)
	}
	link, err := NewClient(account, Options{}).SaveURL(nil, []ObjectRef{{ID: "1.a", ClassID: "1.b"}})
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.MapClaims{}
	if _, err := jwt.NewParser().ParseWithClaims(strings.TrimPrefix(link, SaveURLPrefix), claims, func(*jwt.Token) (any, error) {
		return &key.PublicKey, nil
	}); err != nil {
		t.Fatal(err)
	}
	origins, ok := claims["origins"].([]any)
	if !ok || len(origins) != 0 {
		t.Fatalf("origins %#v", claims["origins"])
	}
}

// fakeGoogle is the token endpoint and the Wallet REST API.
type fakeGoogle struct {
	t      *testing.T
	key    *rsa.PublicKey
	mu     sync.Mutex
	tokens int
	calls  []string
	bodies []string
	status map[string]int
	reply  map[string]string
}

func (g *fakeGoogle) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			g.t.Error(err)
		}
		if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			g.t.Errorf("grant %q", r.PostForm.Get("grant_type"))
		}
		claims := jwt.MapClaims{}
		if _, err := jwt.NewParser(jwt.WithValidMethods([]string{"RS256"})).ParseWithClaims(r.PostForm.Get("assertion"), claims, func(*jwt.Token) (any, error) {
			return g.key, nil
		}); err != nil {
			g.t.Errorf("assertion: %v", err)
		}
		if claims["scope"] != Scope || claims["iss"] != "wallet@skylab-wallet-test.iam.gserviceaccount.com" {
			g.t.Errorf("assertion claims %v", claims)
		}
		if aud, _ := claims["aud"].(string); !strings.HasSuffix(aud, "/token") {
			g.t.Errorf("assertion aud %v", claims["aud"])
		}
		g.mu.Lock()
		g.tokens++
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"ya29.test-token","expires_in":3600,"token_type":"Bearer"}`)
	})
	mux.HandleFunc("/walletobjects/v1/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ya29.test-token" {
			g.t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		call := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/walletobjects/v1")
		g.mu.Lock()
		g.calls = append(g.calls, call)
		g.bodies = append(g.bodies, string(body))
		status, reply := g.status[call], g.reply[call]
		g.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		if reply == "" {
			reply = `{}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	})
	return mux
}

func newFakeClient(t *testing.T, google *fakeGoogle) *Client {
	t.Helper()
	server := httptest.NewServer(google.handler())
	t.Cleanup(server.Close)
	raw, key := testAccount(t, server.URL+"/token")
	google.key = &key.PublicKey
	account, err := parseServiceAccount(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(account, Options{APIBase: server.URL + "/walletobjects/v1", HTTP: server.Client()})
}

func TestClientInsertsAndUpdatesObjectsWithOneCachedToken(t *testing.T) {
	t.Parallel()
	google := &fakeGoogle{t: t}
	client := newFakeClient(t, google)
	ctx := context.Background()
	object := GenericObject{
		ID: "1.sp-A", ClassID: "1.skypass", State: StateActive,
		Header: Localized("tr", "Ada Lovelace"),
	}
	if err := client.InsertGenericObject(ctx, object); err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateGenericObject(ctx, object); err != nil {
		t.Fatal(err)
	}
	google.mu.Lock()
	defer google.mu.Unlock()
	if google.tokens != 1 {
		t.Fatalf("token requests %d, want 1", google.tokens)
	}
	want := []string{"POST /genericObject", "PUT /genericObject/" + url.PathEscape("1.sp-A")}
	if strings.Join(google.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %v", google.calls)
	}
	var sent GenericObject
	if err := json.Unmarshal([]byte(google.bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ID != "1.sp-A" || sent.Header == nil || sent.Header.DefaultValue.Value != "Ada Lovelace" {
		t.Fatalf("sent %s", google.bodies[0])
	}
}

func TestClientMapsConflictAndNotFound(t *testing.T) {
	t.Parallel()
	google := &fakeGoogle{t: t, status: map[string]int{
		"POST /genericObject":    http.StatusConflict,
		"PUT /genericObject/1.x": http.StatusNotFound,
	}}
	client := newFakeClient(t, google)
	ctx := context.Background()
	if err := client.InsertGenericObject(ctx, GenericObject{ID: "1.x", ClassID: "1.c"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("insert %v", err)
	}
	if err := client.UpdateGenericObject(ctx, GenericObject{ID: "1.x", ClassID: "1.c"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update %v", err)
	}
}

func TestEnsureGenericClassPatchesAClassThatExists(t *testing.T) {
	t.Parallel()
	google := &fakeGoogle{t: t, status: map[string]int{"POST /genericClass": http.StatusConflict}}
	client := newFakeClient(t, google)
	class := GenericClass{ID: "1.skypass", MultipleDevicesAndHoldersAllowedStatus: OneUserAllDevices}
	if err := client.EnsureGenericClass(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	google.mu.Lock()
	defer google.mu.Unlock()
	if strings.Join(google.calls, ",") != "POST /genericClass,PATCH /genericClass/1.skypass" {
		t.Fatalf("calls %v", google.calls)
	}
	if !strings.Contains(google.bodies[1], `"multipleDevicesAndHoldersAllowedStatus":"ONE_USER_ALL_DEVICES"`) {
		t.Fatalf("patch body %s", google.bodies[1])
	}
}

func TestAPIErrorsCarryNoSecretMaterial(t *testing.T) {
	t.Parallel()
	secretHex := "3132333435363738393031323334353637383930"
	google := &fakeGoogle{t: t,
		status: map[string]int{"POST /genericObject": http.StatusBadRequest},
		reply: map[string]string{"POST /genericObject": `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"Invalid totp key ` +
			secretHex + ` for object"}}`},
	}
	client := newFakeClient(t, google)
	err := client.InsertGenericObject(context.Background(), GenericObject{ID: "1.x", ClassID: "1.c"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), secretHex) || strings.Contains(err.Error(), "ya29") {
		t.Fatalf("error carries secret material: %v", err)
	}
	if !strings.Contains(err.Error(), "INVALID_ARGUMENT") {
		t.Fatalf("error lost Google's status: %v", err)
	}
}

func TestTokenRefusalIsAnAPIErrorWithoutTheAssertion(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`)
	}))
	t.Cleanup(server.Close)
	raw, _ := testAccount(t, server.URL+"/token")
	account, err := parseServiceAccount(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(account, Options{APIBase: server.URL, HTTP: server.Client()})
	err = client.InsertGenericObject(context.Background(), GenericObject{ID: "1.x", ClassID: "1.c"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Op != "token" || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "eyJ") {
		t.Fatalf("error carries the assertion: %v", err)
	}
}
