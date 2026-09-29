package vault

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVault serves the endpoints the client uses. Tokens it issued are valid until revoked.
type fakeVault struct {
	mu      sync.Mutex
	logins  int
	valid   map[string]bool
	secrets map[string]map[string]any // "mount/path" → data
	lastJWT string
}

func newFakeVault(t *testing.T) (*fakeVault, *httptest.Server) {
	t.Helper()
	f := &fakeVault{valid: map[string]bool{"root": true}, secrets: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeVault) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1/")

	if path == "auth/kubernetes/login" && r.Method == http.MethodPost {
		f.login(w, r)
		return
	}
	if !f.valid[r.Header.Get("X-Vault-Token")] {
		writeStatus(w, http.StatusForbidden, `{"errors":["permission denied"]}`)
		return
	}
	switch {
	case path == "auth/token/lookup-self":
		_, _ = w.Write([]byte(`{"data":{}}`))
	case r.Method == "LIST" && strings.HasPrefix(path, "secret/metadata/"):
		f.list(w, strings.TrimPrefix(path, "secret/metadata/"))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "secret/data/"):
		f.read(w, strings.TrimPrefix(path, "secret/data/"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeVault) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Role, JWT string }
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Role != "aisa" || in.JWT != "sa-jwt" {
		writeStatus(w, http.StatusBadRequest, `{"errors":["permission denied"]}`)
		return
	}
	f.logins++
	f.lastJWT = in.JWT
	token := "k8s-token-" + string(rune('a'+f.logins))
	f.valid[token] = true
	_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": token, "lease_duration": 3600}})
}

func (f *fakeVault) list(w http.ResponseWriter, path string) {
	prefix := "secret/" + path + "/"
	var keys []string
	for k := range f.secrets {
		if name, ok := strings.CutPrefix(k, prefix); ok {
			keys = append(keys, name)
		}
	}
	if len(keys) == 0 {
		writeStatus(w, http.StatusNotFound, `{"errors":[]}`)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": keys}})
}

func (f *fakeVault) read(w http.ResponseWriter, path string) {
	data, ok := f.secrets["secret/"+path]
	if !ok {
		writeStatus(w, http.StatusNotFound, `{"errors":[]}`)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": data}})
}

func writeStatus(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestTokenAuthListAndRead(t *testing.T) {
	f, srv := newFakeVault(t)
	f.secrets["secret/aisa/consumers/chat-ui"] = map[string]any{"key_sha256": "abc"}
	c, err := New(Options{Addr: srv.URL, Auth: TokenAuth{Token: "root"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	keys, err := c.List(ctx, "secret", "aisa/consumers")
	if err != nil || len(keys) != 1 || keys[0] != "chat-ui" {
		t.Fatalf("List = %v, %v", keys, err)
	}
	data, err := c.Read(ctx, "secret", "aisa/consumers/chat-ui")
	if err != nil || data["key_sha256"] != "abc" {
		t.Fatalf("Read = %v, %v", data, err)
	}
	if _, err := c.Read(ctx, "secret", "aisa/consumers/nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read of a missing secret: err = %v, want ErrNotFound", err)
	}
	if _, err := c.List(ctx, "secret", "aisa/empty"); !errors.Is(err, ErrNotFound) {
		t.Errorf("List of an empty path: err = %v, want ErrNotFound", err)
	}
	if err := c.Probe(ctx); err != nil {
		t.Errorf("Probe: %v", err)
	}
}

func TestKubernetesAuthLogsInOnceAndAgainAfterRevocation(t *testing.T) {
	f, srv := newFakeVault(t)
	f.secrets["secret/aisa/consumers/a"] = map[string]any{"x": "1"}
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("sa-jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Addr: srv.URL, Auth: KubernetesAuth{Mount: "kubernetes", Role: "aisa", TokenPath: jwt}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for range 3 {
		if _, err := c.Read(ctx, "secret", "aisa/consumers/a"); err != nil {
			t.Fatal(err)
		}
	}
	if f.logins != 1 {
		t.Errorf("logins = %d after three reads, want 1", f.logins)
	}
	if f.lastJWT != "sa-jwt" {
		t.Errorf("jwt = %q, want the file's content without the newline", f.lastJWT)
	}

	// Revoke all tokens: the next request gets 403, logs in again and succeeds.
	f.mu.Lock()
	f.valid = map[string]bool{}
	f.mu.Unlock()
	if _, err := c.Read(ctx, "secret", "aisa/consumers/a"); err != nil {
		t.Fatalf("read after revocation: %v", err)
	}
	if f.logins != 2 {
		t.Errorf("logins = %d, want 2", f.logins)
	}
}

func TestTokenRenewedBeforeItExpires(t *testing.T) {
	f, srv := newFakeVault(t)
	f.secrets["secret/aisa/consumers/a"] = map[string]any{"x": "1"}
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("sa-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	c, err := New(Options{Addr: srv.URL, Auth: KubernetesAuth{Mount: "kubernetes", Role: "aisa", TokenPath: jwt},
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Read(ctx, "secret", "aisa/consumers/a"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(58 * time.Minute) // lease 60 min, renewed a minute before its end
	if _, err := c.Read(ctx, "secret", "aisa/consumers/a"); err != nil {
		t.Fatal(err)
	}
	if f.logins != 1 {
		t.Errorf("logins = %d at 58 min, want 1", f.logins)
	}
	now = now.Add(90 * time.Second)
	if _, err := c.Read(ctx, "secret", "aisa/consumers/a"); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 {
		t.Errorf("logins = %d at 59.5 min, want 2", f.logins)
	}
}

func TestLoginFailure(t *testing.T) {
	_, srv := newFakeVault(t)
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Addr: srv.URL, Auth: KubernetesAuth{Mount: "kubernetes", Role: "aisa", TokenPath: jwt}})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Probe(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadRequest || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want the login's 400 with Vault's message", err)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Options{Addr: "vault:8200", Auth: TokenAuth{Token: "x"}}); err == nil {
		t.Error("want an error for an address without a scheme")
	}
	if _, err := New(Options{Addr: "http://vault:8200"}); err == nil {
		t.Error("want an error without an auth method")
	}
	if _, err := New(Options{Addr: "https://vault:8200", Auth: TokenAuth{Token: "x"}, CACert: "/no/such/file"}); err == nil {
		t.Error("want an error for a missing CA file")
	}
}
