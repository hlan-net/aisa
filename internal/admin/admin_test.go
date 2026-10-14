package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hlan-net/aisa/internal/apikey"
	"github.com/hlan-net/aisa/internal/consul"
	"github.com/hlan-net/aisa/internal/metrics"
	"github.com/hlan-net/aisa/internal/server"
	"github.com/hlan-net/aisa/internal/vault"
)

type mockConsul struct {
	pairs     map[string][]byte
	instances []consul.Instance
	listErr   error
	putErr    error
	delErr    error
	healthErr error
}

func newMockConsul() *mockConsul {
	return &mockConsul{
		pairs: make(map[string][]byte),
	}
}

func (m *mockConsul) List(ctx context.Context, prefix string, index uint64, wait time.Duration) ([]consul.Pair, uint64, error) {
	if m.listErr != nil {
		return nil, 0, m.listErr
	}
	var out []consul.Pair
	for k, v := range m.pairs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, consul.Pair{Key: k, Value: v})
		}
	}
	return out, 1, nil
}

func (m *mockConsul) Put(ctx context.Context, key string, value []byte) error {
	if m.putErr != nil {
		return m.putErr
	}
	m.pairs[key] = value
	return nil
}

func (m *mockConsul) PutCAS(ctx context.Context, key string, value []byte, cas uint64) error {
	if m.putErr != nil {
		return m.putErr
	}
	if cas == 0 && m.pairs[key] != nil {
		return consul.ErrConflict
	}
	m.pairs[key] = value
	return nil
}

func (m *mockConsul) Delete(ctx context.Context, key string, recurse bool) error {
	if m.delErr != nil {
		return m.delErr
	}
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

func (m *mockConsul) Health(ctx context.Context, service string) ([]consul.Instance, error) {
	if m.healthErr != nil {
		return nil, m.healthErr
	}
	return m.instances, nil
}

type mockVault struct {
	secrets  map[string]map[string]any
	versions map[string]int
	listErr  error
	readErr  error
	writeErr error
	delErr   error
}

func newMockVault() *mockVault {
	return &mockVault{
		secrets:  make(map[string]map[string]any),
		versions: make(map[string]int),
	}
}

func (m *mockVault) List(ctx context.Context, mount, path string) ([]string, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
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
	if m.readErr != nil {
		return nil, m.readErr
	}
	data, ok := m.secrets[mount+"/"+path]
	if !ok {
		return nil, vault.ErrNotFound
	}
	return data, nil
}

func (m *mockVault) ReadVersion(ctx context.Context, mount, path string) (map[string]any, int, error) {
	if m.readErr != nil {
		return nil, 0, m.readErr
	}
	k := mount + "/" + path
	data, ok := m.secrets[k]
	if !ok {
		return nil, 0, vault.ErrNotFound
	}
	return data, m.versions[k], nil
}

func (m *mockVault) Write(ctx context.Context, mount, path string, data map[string]any, cas int) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	k := mount + "/" + path
	currVer := m.versions[k]
	if cas >= 0 && cas != currVer {
		return vault.ErrConflict
	}
	m.secrets[k] = data
	m.versions[k] = currVer + 1
	return nil
}

func (m *mockVault) Delete(ctx context.Context, mount, path string) error {
	if m.delErr != nil {
		return m.delErr
	}
	k := mount + "/" + path
	if _, ok := m.secrets[k]; !ok {
		return vault.ErrNotFound
	}
	delete(m.secrets, k)
	delete(m.versions, k)
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

func newJSONRequest(method, target, body string) *http.Request {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	if method == http.MethodPost || method == http.MethodPut {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestOverviewEndpoint(t *testing.T) {
	h, mc, mv := setupTestHandler()
	_ = mc.Put(context.Background(), "aisa/consumers/chat-ui", []byte(`{"quota_profile":"interactive"}`))
	_ = mv.Write(context.Background(), "secret", "aisa/consumers/chat-ui", map[string]any{"key_sha256": "hash"}, -1)
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

func createTestConsumer(t *testing.T, h *Handler, mc *mockConsul, mv *mockVault) CreateConsumerResponse {
	t.Helper()
	body := `{"name":"test-app","quota_profile":"batch","generate_key":true}`
	req := newJSONRequest(http.MethodPost, "/admin/api/consumers", body)
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
	if !strings.HasPrefix(created.PlaintextKey, apikey.Prefix) {
		t.Errorf("expected key to start with %s, got %s", apikey.Prefix, created.PlaintextKey)
	}
	declBytes := mc.pairs["aisa/consumers/test-app"]
	if !strings.Contains(string(declBytes), "batch") {
		t.Errorf("consul declaration missing: %s", string(declBytes))
	}
	vaultSec := mv.secrets["secret/aisa/consumers/test-app"]
	if vaultSec == nil || vaultSec["key_sha256"] == nil {
		t.Fatal("vault secret missing")
	}
	return created
}

func verifyConsumerList(t *testing.T, h *Handler) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/consumers", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var list []ConsumerItem
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != "test-app" || !list[0].HasKey {
		t.Fatalf("unexpected list: %+v", list)
	}
}

func rotateTestConsumerKey(t *testing.T, h *Handler, oldKey string, mv *mockVault) {
	t.Helper()
	req := newJSONRequest(http.MethodPost, "/admin/api/consumers/test-app/rotate-key", "{}")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate status = %d: %s", w.Code, w.Body.String())
	}
	var rotResp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &rotResp)
	if rotResp["plaintext_key"] == "" || rotResp["plaintext_key"] == oldKey {
		t.Fatalf("new key not generated or same: %v", rotResp)
	}
	// Verify old key hash is superseded in Vault
	vData := mv.secrets["secret/aisa/consumers/test-app"]
	if vData["superseded_sha256"] == nil {
		t.Errorf("expected superseded_sha256 to be present in rotated secret")
	}
}

func deleteTestConsumer(t *testing.T, h *Handler, mc *mockConsul, mv *mockVault) {
	t.Helper()
	req := newJSONRequest(http.MethodDelete, "/admin/api/consumers/test-app", "")
	w := httptest.NewRecorder()
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

func TestConsumersCRUD(t *testing.T) {
	h, mc, mv := setupTestHandler()
	created := createTestConsumer(t, h, mc, mv)
	verifyConsumerList(t, h)
	rotateTestConsumerKey(t, h, created.PlaintextKey, mv)
	deleteTestConsumer(t, h, mc, mv)
}

func TestConsumerConflict(t *testing.T) {
	h, _, _ := setupTestHandler()
	body := `{"name":"dup-app","generate_key":true}`
	req := newJSONRequest(http.MethodPost, "/admin/api/consumers", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("first create status = %d", w.Code)
	}

	// Try creating again with same name: should return 409 Conflict
	req2 := newJSONRequest(http.MethodPost, "/admin/api/consumers", body)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("duplicate create status = %d, want 409", w2.Code)
	}
}

func TestQuotasCRUD(t *testing.T) {
	h, mc, _ := setupTestHandler()

	// 1. Create quota
	body := `{"name":"custom","tokens_per_hour":500000}`
	req := newJSONRequest(http.MethodPost, "/admin/api/quotas", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}

	// 2. Duplicate create -> 409
	reqDup := newJSONRequest(http.MethodPost, "/admin/api/quotas", body)
	wDup := httptest.NewRecorder()
	h.ServeHTTP(wDup, reqDup)
	if wDup.Code != http.StatusConflict {
		t.Fatalf("duplicate quota status = %d, want 409", wDup.Code)
	}

	// 3. List quotas
	reqList := httptest.NewRequest(http.MethodGet, "/admin/api/quotas", nil)
	wList := httptest.NewRecorder()
	h.ServeHTTP(wList, reqList)
	if wList.Code != http.StatusOK {
		t.Fatalf("list status = %d", wList.Code)
	}
	var list []QuotaItem
	_ = json.Unmarshal(wList.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != "custom" || list[0].TokensPerHour != 500000 {
		t.Fatalf("unexpected list: %+v", list)
	}

	// 4. Delete quota
	reqDel := newJSONRequest(http.MethodDelete, "/admin/api/quotas/custom", "")
	wDel := httptest.NewRecorder()
	h.ServeHTTP(wDel, reqDel)
	if wDel.Code != http.StatusOK {
		t.Fatalf("delete status = %d", wDel.Code)
	}
	if mc.pairs["aisa/quotas/custom"] != nil {
		t.Error("expected quota deleted from Consul")
	}
}

func TestBackendsAndModels(t *testing.T) {
	h, mc, _ := setupTestHandler()
	mc.instances = []consul.Instance{
		{
			ID:      "local-ollama",
			Service: "aisa-backend",
			Address: "127.0.0.1",
			Port:    11434,
			Tags:    []string{"mock"},
			Meta: map[string]string{
				"provider": "openai-compatible",
				"models":   "qwen3, llama3.2",
				"priority": "1",
			},
			Passing: true,
			Checks: []consul.Check{
				{CheckID: "service:local-ollama", Status: "passing"},
			},
		},
	}

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

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMovedPermanently {
		t.Errorf("status = %d, want 301", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "aisa // Administration") {
		t.Errorf("body missing title")
	}

	// Verify JS asset is served
	req = httptest.NewRequest(http.MethodGet, "/admin/assets/app.js", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("js status = %d, want 200", w.Code)
	}
}

func TestConsumerValidation(t *testing.T) {
	h, _, _ := setupTestHandler()

	tests := []struct {
		name string
		body string
		code int
	}{
		{"invalid json", `{"name":`, http.StatusBadRequest},
		{"empty name", `{"name":""}`, http.StatusBadRequest},
		{"name with spaces", `{"name":"bad name"}`, http.StatusBadRequest},
		{"name with slash", `{"name":"bad/name"}`, http.StatusBadRequest},
		{"name with quotes (XSS test)", `{"name":"bad'name"}`, http.StatusBadRequest},
		{"name with script tags (XSS test)", `{"name":"<script>"}`, http.StatusBadRequest},
		{"name unknown", `{"name":"unknown"}`, http.StatusBadRequest},
		{"namespace without secret", `{"name":"app","namespace":"ns"}`, http.StatusBadRequest},
		{"secret without namespace", `{"name":"app","secret":"sec"}`, http.StatusBadRequest},
		{"k8s secret with generate_key refused", `{"name":"app","namespace":"ns","secret":"sec","generate_key":true}`, http.StatusBadRequest},
		{"k8s secret with custom_key refused", `{"name":"app","namespace":"ns","secret":"sec","custom_key":"1234567890123456"}`, http.StatusBadRequest},
		{"custom key too short", `{"name":"app","custom_key":"short"}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newJSONRequest(http.MethodPost, "/admin/api/consumers", tt.body)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tt.code {
				t.Errorf("status = %d, want %d: %s", w.Code, tt.code, w.Body.String())
			}
		})
	}
}

func TestConsumerCustomKey(t *testing.T) {
	h, _, mv := setupTestHandler()
	body := `{"name":"custom-app","custom_key":"my-custom-key-16-chars-long"}`
	req := newJSONRequest(http.MethodPost, "/admin/api/consumers", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}
	var resp CreateConsumerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.PlaintextKey != "my-custom-key-16-chars-long" {
		t.Errorf("unexpected resp: %+v", resp)
	}
	if mv.secrets["secret/aisa/consumers/custom-app"] == nil {
		t.Fatal("expected vault secret for custom key")
	}
}

func TestConsumerDeleteAndRotateErrors(t *testing.T) {
	h, mc, mv := setupTestHandler()

	// Rotate non-existent consumer -> 404
	req := newJSONRequest(http.MethodPost, "/admin/api/consumers/non-existent/rotate-key", "{}")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("rotate non-existent: status = %d, want 404", w.Code)
	}

	// Rotate K8s-managed consumer -> 400
	_ = mc.Put(context.Background(), "aisa/consumers/k8s-app", []byte(`{"namespace":"prod","secret":"sec"}`))
	req = newJSONRequest(http.MethodPost, "/admin/api/consumers/k8s-app/rotate-key", "{}")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("rotate k8s consumer: status = %d, want 400", w.Code)
	}

	// Delete non-existent consumer -> 404
	req = newJSONRequest(http.MethodDelete, "/admin/api/consumers/non-existent", "")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("delete non-existent: status = %d, want 404", w.Code)
	}

	// Delete consumer when Vault delete fails -> 500, Consul is NOT deleted
	_ = mc.Put(context.Background(), "aisa/consumers/vault-fail", []byte(`{}`))
	_ = mv.Write(context.Background(), "secret", "aisa/consumers/vault-fail", map[string]any{"key_sha256": "h"}, -1)
	mv.delErr = errors.New("vault permission denied")
	req = newJSONRequest(http.MethodDelete, "/admin/api/consumers/vault-fail", "")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("delete vault fail: status = %d, want 500", w.Code)
	}
	if mc.pairs["aisa/consumers/vault-fail"] == nil {
		t.Error("expected consumer in Consul to remain when Vault delete fails")
	}
	mv.delErr = nil
}

func TestCSRFAndContentType(t *testing.T) {
	h, _, _ := setupTestHandler()

	// Missing Content-Type on POST -> 415
	req := httptest.NewRequest(http.MethodPost, "/admin/api/consumers", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected 415 on missing Content-Type, got %d", w.Code)
	}

	// Sec-Fetch-Site: cross-site -> 403
	req2 := newJSONRequest(http.MethodPost, "/admin/api/consumers", `{"name":"cs-app"}`)
	req2.Header.Set("Sec-Fetch-Site", "cross-site")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Errorf("expected 403 on cross-site, got %d", w2.Code)
	}
}

func TestVaultListErrorPropagated(t *testing.T) {
	h, _, mv := setupTestHandler()
	mv.listErr = errors.New("vault is down")

	req := httptest.NewRequest(http.MethodGet, "/admin/api/consumers", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on vault list error, got %d", w.Code)
	}
}

func TestOverviewDegraded(t *testing.T) {
	mc := newMockConsul()
	mv := newMockVault()
	met := metrics.New("v0.2.0-test")
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	check := server.Check{
		Name:  "failing-check",
		Probe: func(context.Context) error { return errors.New("check failed") },
	}
	h := New(mc, mv, met, log, Options{
		ConsulPrefix: "aisa",
		VaultMount:   "secret",
		VaultPrefix:  "aisa",
	}, check)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp OverviewResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Ready {
		t.Errorf("ready = true, want false")
	}
	if resp.Status != "degraded" {
		t.Errorf("status = %s, want degraded", resp.Status)
	}
}
