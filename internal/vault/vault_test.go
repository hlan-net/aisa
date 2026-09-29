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
	// denied are paths that the policy of every token forbids.
	denied map[string]bool
	// notFound, when set, is the body of a 404 that answers every request for data.
	notFound string
	// ok, when set, is the body of a 200 that answers every request for data.
	ok      string
	lookups int
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
		f.lookups++
		_, _ = w.Write([]byte(`{"data":{}}`))
	case f.denied[path]:
		writeStatus(w, http.StatusForbidden, `{"errors":["1 error occurred:\n\t* permission denied\n\n"]}`)
	case f.notFound != "":
		writeStatus(w, http.StatusNotFound, f.notFound)
	case f.ok != "":
		_, _ = w.Write([]byte(f.ok))
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

func TestNotFoundIsOnlyWhatVaultSaysAboutItsData(t *testing.T) {
	f, srv := newFakeVault(t)
	c, err := New(Options{Addr: srv.URL, Auth: TokenAuth{Token: "root"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for name, tc := range map[string]struct {
		body     string
		notFound bool
	}{
		"a path without data":         {`{"errors":[]}`, true},
		"a deleted version":           {`{"data":{"data":null,"metadata":{"deletion_time":"2026-01-01T00:00:00Z"}}}`, true},
		"a destroyed version":         {`{"data":{"data":null,"metadata":{"deletion_time":"","destroyed":true}}}`, true},
		"a mount that does not exist": {`{"errors":["no handler for route \"nomount/metadata/aisa/consumers/\". route entry not found."]}`, false},
		"a proxy in front of Vault":   {`<html><body>404 Not Found</body></html>`, false},
		"a proxy's empty object":      {`{}`, false},
		"a proxy's JSON null":         {`null`, false},
		"a proxy's JSON message":      {`{"message":"not found"}`, false},
		"errors that are null":        {`{"errors":null}`, false},
		"an empty answer":             {``, false},
	} {
		f.mu.Lock()
		f.notFound = tc.body
		f.mu.Unlock()
		if tc.body == "" {
			// The fake answers from its data when notFound is empty; a bare 404 comes from the default.
			_, err = c.List(ctx, "nomount", "aisa/consumers")
		} else {
			_, err = c.List(ctx, "secret", "aisa/consumers")
		}
		if got := errors.Is(err, ErrNotFound); got != tc.notFound {
			t.Errorf("%s: ErrNotFound = %v, want %v (err: %v)", name, got, tc.notFound, err)
		}
		var se *StatusError
		if !tc.notFound && (!errors.As(err, &se) || se.Code != http.StatusNotFound) {
			t.Errorf("%s: err = %v, want a StatusError with 404", name, err)
		}
	}
}

func TestASuccessWithoutVaultsFieldsIsAnError(t *testing.T) {
	f, srv := newFakeVault(t)
	c, err := New(Options{Addr: srv.URL, Auth: TokenAuth{Token: "root"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, body := range []string{`{}`, `null`, `{"data":null}`, `{"data":{}}`, `{"data":{"keys":null}}`} {
		f.mu.Lock()
		f.ok = body
		f.mu.Unlock()
		if keys, err := c.List(ctx, "secret", "aisa/consumers"); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("List %s: %v, %v, want an error other than ErrNotFound", body, keys, err)
		}
		if data, err := c.Read(ctx, "secret", "aisa/consumers/a"); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Read %s: %v, %v, want an error other than ErrNotFound", body, data, err)
		}
	}

	f.mu.Lock()
	f.ok = `{"data":{"keys":[]}}`
	f.mu.Unlock()
	if keys, err := c.List(ctx, "secret", "aisa/consumers"); err != nil || len(keys) != 0 {
		t.Errorf("an empty list from Vault: %v, %v", keys, err)
	}
	f.mu.Lock()
	f.ok = `{"data":{"data":null,"metadata":{"deletion_time":"2026-01-01T00:00:00Z"}}}`
	f.mu.Unlock()
	if _, err := c.Read(ctx, "secret", "aisa/consumers/a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted version answered with 200: %v, want ErrNotFound", err)
	}
}

func TestNamesAreEscaped(t *testing.T) {
	f, srv := newFakeVault(t)
	f.secrets["secret/aisa/consumers/a"] = map[string]any{"owner": "a"}
	names := []string{"a?b", "a#b", "50%", "with space", "a%3Fb"}
	for _, n := range names {
		f.secrets["secret/aisa/consumers/"+n] = map[string]any{"owner": n}
	}
	c, err := New(Options{Addr: srv.URL, Auth: TokenAuth{Token: "root"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		data, err := c.Read(context.Background(), "secret", "aisa/consumers/"+n)
		if err != nil {
			t.Errorf("%q: %v", n, err)
			continue
		}
		if data["owner"] != n {
			t.Errorf("%q: read the secret of %q", n, data["owner"])
		}
	}
}

func TestDeniedByPolicyDoesNotLogInAgain(t *testing.T) {
	f, srv := newFakeVault(t)
	f.denied = map[string]bool{"secret/metadata/aisa/consumers": true}
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("sa-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Addr: srv.URL, Auth: KubernetesAuth{Mount: "kubernetes", Role: "aisa", TokenPath: jwt}})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		_, err := c.List(context.Background(), "secret", "aisa/consumers")
		var se *StatusError
		if !errors.As(err, &se) || se.Code != http.StatusForbidden {
			t.Fatalf("err = %v, want Vault's 403", err)
		}
	}
	if f.logins != 1 {
		t.Errorf("logins = %d after five denied requests, want 1: each login leaves a token in Vault", f.logins)
	}
}

func TestFixedTokenThatVaultRejects(t *testing.T) {
	f, srv := newFakeVault(t)
	c, err := New(Options{Addr: srv.URL, Auth: TokenAuth{Token: "revoked"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Read(context.Background(), "secret", "aisa/consumers/a")
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Errorf("err = %v, want Vault's 403", err)
	}
	if f.logins != 0 {
		t.Errorf("logins = %d, want 0 with a fixed token", f.logins)
	}
}

func TestOnlyOneLoginWhenSeveralRequestsFindTheTokenRevoked(t *testing.T) {
	f, srv := newFakeVault(t)
	f.secrets["secret/aisa/consumers/a"] = map[string]any{"x": "1"}
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("sa-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Addr: srv.URL, Auth: KubernetesAuth{Mount: "kubernetes", Role: "aisa", TokenPath: jwt}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Read(ctx, "secret", "aisa/consumers/a"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.valid = map[string]bool{}
	f.mu.Unlock()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := c.Read(ctx, "secret", "aisa/consumers/a"); err != nil {
				t.Errorf("read after revocation: %v", err)
			}
		})
	}
	wg.Wait()
	if f.logins != 2 {
		t.Errorf("logins = %d, want 2: the first one and one after the revocation", f.logins)
	}
}
