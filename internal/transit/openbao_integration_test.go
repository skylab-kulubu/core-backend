package transit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/transit"
)

// openBaoImage is the OpenBao the sandbox and production run.
const openBaoImage = "openbao/openbao:2.7.0"

// startOpenBao runs a disposable OpenBao dev server (in-memory storage, root
// token "root") and removes it when the test ends. The test is skipped when
// Docker or the image cannot be used.
func startOpenBao(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("core-openbao-test-%d", time.Now().UnixNano())
	// Dev mode keeps everything in memory and the image declares no VOLUME,
	// so the container leaves no volume behind. (A tmpfs over /openbao/*
	// breaks the entrypoint's chown.)
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", "127.0.0.1::8200",
		openBaoImage, "server", "-dev", "-dev-no-store-token", "-dev-root-token-id=root", "-dev-listen-address=0.0.0.0:8200",
	)
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("docker run openbao: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() })

	var addr string
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := exec.Command("docker", "port", name, "8200/tcp").CombinedOutput()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			addr = "http://" + strings.TrimSpace(strings.Split(string(out), "\n")[0])
			resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(addr + "/v1/sys/health")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return addr
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("openbao never became ready: %s", out)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// bao calls OpenBao as root.
func bao(t *testing.T, addr, method, path string, body any) map[string]any {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, addr+"/v1/"+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Vault-Token", "root")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var failure map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&failure)
		t.Fatalf("%s %s: %d %v", method, path, resp.StatusCode, failure)
	}
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// The client against a real OpenBao set up as the wizard sets it up: a
// Transit mount with a media key, a policy granting only encrypt, decrypt
// and rewrap, and an AppRole with a one-hour renewable token.
func TestClientAgainstARealOpenBao(t *testing.T) {
	t.Parallel()
	addr := startOpenBao(t)
	bao(t, addr, http.MethodPost, "sys/mounts/transit/it", map[string]any{"type": "transit"})
	bao(t, addr, http.MethodPost, "transit/it/keys/media", map[string]any{"type": "aes256-gcm96"})
	// A mount where the key was never made (the wizard's disable_upsert).
	bao(t, addr, http.MethodPost, "sys/mounts/transit/empty", map[string]any{"type": "transit"})
	bao(t, addr, http.MethodPost, "transit/empty/config/keys", map[string]any{"disable_upsert": true})
	bao(t, addr, http.MethodPut, "sys/policies/acl/core-media-it", map[string]any{"policy": `
path "transit/it/encrypt/media" { capabilities = ["update"] }
path "transit/it/decrypt/media" { capabilities = ["update"] }
path "transit/it/rewrap/media" { capabilities = ["update"] }
path "transit/empty/encrypt/media" { capabilities = ["update"] }
path "transit/empty/decrypt/media" { capabilities = ["update"] }`})
	bao(t, addr, http.MethodPost, "sys/auth/approle", map[string]any{"type": "approle"})
	bao(t, addr, http.MethodPost, "auth/approle/role/core-media-it", map[string]any{
		"token_policies": []string{"core-media-it"}, "token_ttl": "1h", "token_max_ttl": "24h", "token_type": "service",
	})
	roleID := bao(t, addr, http.MethodGet, "auth/approle/role/core-media-it/role-id", nil)["data"].(map[string]any)["role_id"].(string)
	secretID := bao(t, addr, http.MethodPost, "auth/approle/role/core-media-it/secret-id", map[string]any{})["data"].(map[string]any)["secret_id"].(string)

	client := transit.New(transit.Config{Addr: addr, Mount: "transit/it", Key: "media", RoleID: roleID, SecretID: secretID})
	ctx := context.Background()
	before, version, err := client.WrapKey(ctx, dataKey())
	if err != nil || version != 1 {
		t.Fatalf("wrap: version %d, err %v", version, err)
	}

	bao(t, addr, http.MethodPost, "transit/it/keys/media/rotate", map[string]any{})
	after, version, err := client.WrapKey(ctx, dataKey())
	if err != nil || version != 2 {
		t.Fatalf("wrap after rotation: version %d, err %v", version, err)
	}
	for _, wrapped := range []string{before, after} {
		got, err := client.UnwrapKey(ctx, wrapped)
		if err != nil || !bytes.Equal(got, dataKey()) {
			t.Fatalf("unwrap %.9s: %v", wrapped, err)
		}
	}

	// Without the key, encrypt would create it, which the policy does not
	// grant: a refusal right after a fresh login; decrypt says the key is
	// not found. Both are OpenBao's configuration.
	empty := transit.New(transit.Config{Addr: addr, Mount: "transit/empty", Key: "media", RoleID: roleID, SecretID: secretID})
	if _, _, err := empty.WrapKey(ctx, dataKey()); !errors.Is(err, transit.ErrMisconfigured) {
		t.Fatalf("encrypt on a mount without the key: err = %v", err)
	}
	empty = transit.New(transit.Config{Addr: addr, Mount: "transit/empty", Key: "media", RoleID: roleID, SecretID: secretID})
	if _, err := empty.UnwrapKey(ctx, before); !errors.Is(err, transit.ErrMisconfigured) {
		t.Fatalf("decrypt on a mount without the key: err = %v", err)
	}

	wrong := transit.New(transit.Config{Addr: addr, Mount: "transit/it", Key: "media", RoleID: roleID, SecretID: "wrong"})
	if _, _, err := wrong.WrapKey(ctx, dataKey()); !errors.Is(err, transit.ErrDenied) {
		t.Fatalf("a wrong secret_id: err = %v", err)
	}
}
