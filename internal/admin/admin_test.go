package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hlan-net/aisa/internal/consul"
	"github.com/hlan-net/aisa/internal/metrics"
	"github.com/hlan-net/aisa/internal/server"
	"github.com/hlan-net/aisa/internal/vault"
)

type mockConsul struct {
	pairs    map[string][]byte
	backends []consul.ServiceHealth
}

func newMockConsul() *mockConsul {
	return &mockConsul{
		pairs: make(map[string][]byte),
	}
}

func (m *mockConsul) List(ctx context.Context, prefix string, index uint64, wait time.Duration) ([]consul.Pair, uint64, error) {
	var out []consul.Pair
	for k, v := range m.pairs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, consul.Pair{Key: k, Value: v})
		}
	}
	return out, 1, nil
}

func (m *mockConsul) Put(ctx context.Context, key string, value []byte) error {
	m.pairs[key] = value
	return nil
}

func (m *mockConsul) Delete(ctx context.Context, key string, recurse bool) error {
	if recurse {
		for k := range m.pairs {
			if strings.HasPrefix(k, key) {
				delete(m.pairs, k)
			}
		}
		return nil
	}
	delete(m.pairs, key)
	return nil
}

func (m *mockConsul) HealthService(ctx context.Context, service string, passingOnly bool) ([]consul.ServiceHealth, error) {
	return m.backends, nil
}

type mockVault struct {
	secrets map[string]map[string]any
}

func newMockVault() *mockVault {
	return &mockVault{secrets: make(map[string]map[string]any)}
}

func (m *mockVault) List(ctx context.Context, mount, path string) ([]string, error) {
	prefix := mount + "/" + path + "/"
	keys := make(map[string]bool)
	for k := range m.secrets {
		if strings.HasPrefix(k, prefix) {
			name := strings.TrimPrefix(k, prefix)
			parts := strings.Split(name, "/")
			keys[parts[0]] = true
		}
	}
	var res []string
	for k := range keys {
		res = append(res, k)
	}
	return res, nil
}

func (m *mockVault) Read(ctx context.Context, mount, path string) (map[string]any, error) {
	data, ok := m.secrets[mount+"/"+path]
	if !ok {
		return nil, vault.ErrNotFound
	}
	return data, nil
}

func (m *mockVault) Write(ctx context.Context, mount, path string, data map[string]any) error {
	m.secrets[mount+"/"+path] = data
	return nil
}

func (m *mockVault) Delete(ctx context.Context, mount, path string) error {
	delete(m.secrets, mount+"/"+path)
	return nil
}

func (m *mockVault) Probe(ctx context.Context) error {
	return nil
}

func setupTestHandler() (*Handler, *mockConsul, *mockVault) {
	mc := newMockConsul()
	mv := newMockVault()
	met := metrics.New("v0.2.0-test")
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	check := server.Check{
		Name:  "test-check",
		Probe: func(context.Context) error { return nil },
	}
	h := New(mc, mv, met, log, Options{
		ConsulPrefix: "aisa",
		VaultMount:   "secret",
		VaultPrefix:  "aisa",
	}, check)
	return h, mc, mv
}

func TestOverviewEndpoint(t *testing.T) {
	h, mc, mv := setupTestHandler()
	// Seed one consumer and one quota
	_ = mc.Put(context.Background(), "aisa/consumers/chat-ui", []byte(`{"quota_profile":"interactive"}`))
	_ = mv.Write(context.Background(), "secret", "aisa/consumers/chat-ui", map[string]any{"key_sha256": "hash"})
	_ = mc.Put(context.Background(), "aisa/quotas/interactive", []byte(`{"tokens_per_hour":1000000}`))

	req := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp OverviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Ready || resp.Status != "ok" {
		t.Errorf("ready=%v, status=%s", resp.Ready, resp.Status)
	}
	if resp.Counts.Consumers != 1 || resp.Counts.Keys != 1 || resp.Counts.Quotas != 1 {
		t.Errorf("unexpected counts: %+v", resp.Counts)
	}
}

func TestConsumersCRUD(t *testing.T) {
	h, mc, mv := setupTestHandler()

	// 1. Create consumer with auto-generated key
	body := `{"name":"test-app","quota_profile":"batch","generate_key":true}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/consumers", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}
	var created CreateConsumerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "test-app" || !created.HasKey || created.PlaintextKey == "" {
		t.Fatalf("unexpected created response: %+v", created)
	}
	if !strings.HasPrefix(created.PlaintextKey, "aisa_sec_") {
		t.Errorf("expected key to start with aisa_sec_, got %s", created.PlaintextKey)
	}

	// Verify written in Consul and Vault
	declBytes := mc.pairs["aisa/consumers/test-app"]
	if !strings.Contains(string(declBytes), "batch") {
		t.Errorf("consul declaration missing: %s", string(declBytes))
	}
	vaultSec := mv.secrets["secret/aisa/consumers/test-app"]
	if vaultSec == nil || vaultSec["key_sha256"] == nil {
		t.Fatal("vault secret missing")
	}

	// 2. List consumers
	req = httptest.NewRequest(http.MethodGet, "/admin/api/consumers", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var list []ConsumerItem
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != "test-app" || !list[0].HasKey {
		t.Fatalf("unexpected list: %+v", list)
	}

	// 3. Rotate key
	req = httptest.NewRequest(http.MethodPost, "/admin/api/consumers/test-app/rotate-key", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate status = %d: %s", w.Code, w.Body.String())
	}
	var rotResp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &rotResp)
	if rotResp["plaintext_key"] == "" || rotResp["plaintext_key"] == created.PlaintextKey {
		t.Fatalf("new key not generated or same: %v", rotResp)
	}

	// 4. Delete consumer
	req = httptest.NewRequest(http.MethodDelete, "/admin/api/consumers/test-app", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d", w.Code)
	}
	if mc.pairs["aisa/consumers/test-app"] != nil {
		t.Error("expected consumer deleted from Consul")
	}
	if mv.secrets["secret/aisa/consumers/test-app"] != nil {
		t.Error("expected consumer deleted from Vault")
	}
}

func TestQuotasCRUD(t *testing.T) {
	h, mc, _ := setupTestHandler()

	// 1. Create quota
	body := `{"name":"custom","tokens_per_hour":500000}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/quotas", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}

	// 2. List quotas
	req = httptest.NewRequest(http.MethodGet, "/admin/api/quotas", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var list []QuotaItem
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != "custom" || list[0].TokensPerHour != 500000 {
		t.Fatalf("unexpected list: %+v", list)
	}

	// 3. Delete quota
	req = httptest.NewRequest(http.MethodDelete, "/admin/api/quotas/custom", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d", w.Code)
	}
	if mc.pairs["aisa/quotas/custom"] != nil {
		t.Error("expected quota deleted from Consul")
	}
}

func TestBackendsAndModels(t *testing.T) {
	h, mc, _ := setupTestHandler()
	mc.backends = []consul.ServiceHealth{
		{
			Service: consul.ServiceInfo{
				ID:      "local-ollama",
				Service: "aisa-backend",
				Address: "127.0.0.1",
				Port:    11434,
				Meta: map[string]string{
					"provider": "openai-compatible",
					"models":   "qwen3, llama3.2",
					"priority": "1",
				},
			},
			Checks: []consul.ServiceCheck{
				{CheckID: "service:local-ollama", Status: "passing"},
			},
		},
	}

	// Backends
	req := httptest.NewRequest(http.MethodGet, "/admin/api/backends", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("backends status = %d", w.Code)
	}
	var backends []BackendItem
	_ = json.Unmarshal(w.Body.Bytes(), &backends)
	if len(backends) != 1 || backends[0].ID != "local-ollama" || len(backends[0].Models) != 2 {
		t.Fatalf("unexpected backends: %+v", backends)
	}

	// Models
	req = httptest.NewRequest(http.MethodGet, "/admin/api/models", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("models status = %d", w.Code)
	}
	var models []ModelItem
	_ = json.Unmarshal(w.Body.Bytes(), &models)
	if len(models) != 2 || models[0].Name != "llama3.2" || models[1].Name != "qwen3" {
		t.Fatalf("unexpected models: %+v", models)
	}
}

func TestWebUIAssets(t *testing.T) {
	h, _, _ := setupTestHandler()

	// GET /admin redirects to /admin/
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMovedPermanently {
		t.Errorf("status = %d, want 301", w.Code)
	}

	// GET /admin/ serves index.html
	req = httptest.NewRequest(http.MethodGet, "/admin/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "aisa // Administration") {
		t.Errorf("body missing title: %s", w.Body.String()[:min(len(w.Body.String()), 200)])
	}

	requiredIDs := []string{
		"version-tag",
		"system-status-indicator",
		"system-status-text",
		"stat-consumers",
		"stat-keys",
		"stat-quotas",
		"stat-backends",
		"stat-models",
		"readiness-list",
		"consumers-table-body",
		"consumer-quota",
		"quotas-table-body",
		"quota-tokens",
		"quota-name",
		"modal-add-quota",
		"form-add-quota",
		"backends-table-body",
		"models-list",
		"consumer-gen-key",
		"custom-key-container",
		"consumer-name",
		"consumer-namespace",
		"consumer-secret",
		"consumer-custom-key",
		"modal-add-consumer",
		"form-add-consumer",
		"revealed-key-text",
		"modal-key-reveal",
		"toast",
	}
	htmlBody := w.Body.String()
	for _, id := range requiredIDs {
		if !strings.Contains(htmlBody, `id="`+id+`"`) {
			t.Errorf("index.html missing required DOM element id=%q", id)
		}
	}

	// GET /admin/assets/style.css serves CSS
	req = httptest.NewRequest(http.MethodGet, "/admin/assets/style.css", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("css status = %d, want 200", w.Code)
	}

	// GET /admin/assets/app.js serves JavaScript
	req = httptest.NewRequest(http.MethodGet, "/admin/assets/app.js", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("js status = %d, want 200", w.Code)
	}
}
