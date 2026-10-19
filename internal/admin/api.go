package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hlan-net/aisa/internal/apikey"
	"github.com/hlan-net/aisa/internal/consul"
	"github.com/hlan-net/aisa/internal/metrics"
	"github.com/hlan-net/aisa/internal/server"
	"github.com/hlan-net/aisa/internal/vault"
	"github.com/hlan-net/aisa/internal/version"
)

var validNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func isValidName(name string) bool {
	return len(name) >= 1 && len(name) <= 253 &&
		name != "." && name != ".." &&
		validNameRE.MatchString(name) &&
		name != metrics.ConsumerUnknown
}

func findExactKVPair(pairs []consul.Pair, exactKey string) *consul.Pair {
	for i := range pairs {
		if pairs[i].Key == exactKey {
			return &pairs[i]
		}
	}
	return nil
}

// OverviewResponse is the summary information returned by GET /admin/api/overview.
type OverviewResponse struct {
	Version string            `json:"version"`
	Status  string            `json:"status"`
	Ready   bool              `json:"ready"`
	Checks  map[string]string `json:"checks"`
	Counts  OverviewCounts    `json:"counts"`
}

type OverviewCounts struct {
	Consumers int `json:"consumers"`
	Keys      int `json:"keys"`
	Quotas    int `json:"quotas"`
	Backends  int `json:"backends"`
	Models    int `json:"models"`
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	errs := make([]error, len(h.checks))
	var wg sync.WaitGroup
	for i, c := range h.checks {
		wg.Go(func() { errs[i] = server.ProbeWithin(ctx, c) })
	}
	wg.Wait()

	ready := true
	checks := make(map[string]string, len(h.checks))
	for i, c := range h.checks {
		if errs[i] != nil {
			checks[c.Name] = "failed"
			ready = false
		} else {
			checks[c.Name] = "ok"
		}
	}

	// Count consumers
	consumers, _ := h.fetchConsumers(ctx)
	quotas, _ := h.fetchQuotas(ctx)
	backends, _ := h.fetchBackends(ctx)

	// Count unique models
	modelSet := make(map[string]bool)
	for _, b := range backends {
		for _, m := range b.Models {
			modelSet[m] = true
		}
	}

	keyCount := 0
	for _, c := range consumers {
		if c.HasKey {
			keyCount++
		}
	}

	status := "ok"
	if !ready {
		status = "degraded"
	}

	resp := OverviewResponse{
		Version: version.Version,
		Status:  status,
		Ready:   ready,
		Checks:  checks,
		Counts: OverviewCounts{
			Consumers: len(consumers),
			Keys:      keyCount,
			Quotas:    len(quotas),
			Backends:  len(backends),
			Models:    len(modelSet),
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

const (
	pathConsumers           = "/consumers/"
	pathQuotas              = "/quotas/"
	errConsumerNameRequired = "consumer name is required"
	errInvalidConsumerName  = "invalid consumer name"
	errQuotaNameRequired    = "quota profile name is required"
	errConsulListPrefix     = "consul list: "
)

// ConsumerItem represents a consumer declaration and Vault status.
type ConsumerItem struct {
	Name         string `json:"name"`
	QuotaProfile string `json:"quota_profile"`
	Namespace    string `json:"namespace,omitempty"`
	Secret       string `json:"secret,omitempty"`
	HasKey       bool   `json:"has_key"`
	Status       string `json:"status"` // ready, undeclared, missing_key, invalid
}

func parseConsumerItem(p consul.Pair, prefix string, vaultKeys map[string]bool) (ConsumerItem, bool) {
	name := strings.TrimPrefix(p.Key, prefix)
	if name == "" || strings.HasSuffix(name, "/") {
		return ConsumerItem{}, false
	}
	var decl struct {
		QuotaProfile string `json:"quota_profile"`
		Namespace    string `json:"namespace"`
		Secret       string `json:"secret"`
	}
	status := "ready"
	if err := json.Unmarshal(p.Value, &decl); err != nil {
		status = "invalid"
	}
	hasKey := vaultKeys[name]
	if !hasKey && status == "ready" {
		status = "missing_key"
	}
	return ConsumerItem{
		Name:         name,
		QuotaProfile: decl.QuotaProfile,
		Namespace:    decl.Namespace,
		Secret:       decl.Secret,
		HasKey:       hasKey,
		Status:       status,
	}, true
}

func appendUndeclaredVaultKeys(items []ConsumerItem, vaultKeys map[string]bool) []ConsumerItem {
	consulNames := make(map[string]bool, len(items))
	for _, it := range items {
		consulNames[it.Name] = true
	}
	for vk := range vaultKeys {
		if !consulNames[vk] {
			items = append(items, ConsumerItem{
				Name:   vk,
				HasKey: true,
				Status: "undeclared",
			})
		}
	}
	return items
}

func (h *Handler) fetchConsumers(ctx context.Context) ([]ConsumerItem, error) {
	prefix := h.opts.ConsulPrefix + pathConsumers
	pairs, _, err := h.consul.List(ctx, prefix, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("list consul consumers: %w", err)
	}

	vaultKeys := make(map[string]bool)
	if h.vault != nil {
		keys, err := h.vault.List(ctx, h.opts.VaultMount, strings.TrimSuffix(h.opts.VaultPrefix+pathConsumers, "/"))
		if err != nil && !errors.Is(err, vault.ErrNotFound) {
			return nil, fmt.Errorf("list vault consumers: %w", err)
		}
		for _, k := range keys {
			vaultKeys[strings.TrimSuffix(k, "/")] = true
		}
	}

	items := make([]ConsumerItem, 0, len(pairs))
	for _, p := range pairs {
		if item, ok := parseConsumerItem(p, prefix, vaultKeys); ok {
			items = append(items, item)
		}
	}

	items = appendUndeclaredVaultKeys(items, vaultKeys)
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

func (h *Handler) listConsumers(w http.ResponseWriter, r *http.Request) {
	items, err := h.fetchConsumers(r.Context())
	if err != nil {
		h.log.Error("list consumers failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list consumers: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// CreateConsumerRequest is the payload for POST /admin/api/consumers.
type CreateConsumerRequest struct {
	Name         string `json:"name"`
	QuotaProfile string `json:"quota_profile"`
	Namespace    string `json:"namespace"`
	Secret       string `json:"secret"`
	GenerateKey  bool   `json:"generate_key"`
	CustomKey    string `json:"custom_key"`
}

// CreateConsumerResponse is returned on successful consumer creation.
type CreateConsumerResponse struct {
	ConsumerItem
	PlaintextKey string `json:"plaintext_key,omitempty"`
	Message      string `json:"message,omitempty"`
}

func validateCreateConsumer(req CreateConsumerRequest) error {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return errors.New(errConsumerNameRequired)
	}
	if !isValidName(name) {
		return errors.New(errInvalidConsumerName)
	}
	if (req.Namespace == "") != (req.Secret == "") {
		return errors.New("namespace and secret must be specified together")
	}
	if req.Namespace != "" && (req.GenerateKey || req.CustomKey != "") {
		return errors.New("keys for Kubernetes-managed consumers (with namespace/secret) are managed by Kubernetes Secrets; manual key issuance is not permitted")
	}
	if req.CustomKey != "" && len(strings.TrimSpace(req.CustomKey)) < apikey.MinCustomKeyLength {
		return apikey.ErrKeyTooShort
	}
	return nil
}

func encodeConsumerDeclaration(req CreateConsumerRequest) ([]byte, error) {
	decl := map[string]string{}
	if req.QuotaProfile != "" {
		decl["quota_profile"] = strings.TrimSpace(req.QuotaProfile)
	}
	if req.Namespace != "" && req.Secret != "" {
		decl["namespace"] = strings.TrimSpace(req.Namespace)
		decl["secret"] = strings.TrimSpace(req.Secret)
	}
	return json.Marshal(decl)
}

func resolveConsumerKey(req CreateConsumerRequest) (plaintext, keyHash string, err error) {
	if req.GenerateKey {
		plaintext, err = apikey.New(nil)
		if err != nil {
			return "", "", fmt.Errorf("generate key: %w", err)
		}
		return plaintext, apikey.Hash(plaintext), nil
	}
	if req.CustomKey != "" {
		plaintext = strings.TrimSpace(req.CustomKey)
		return plaintext, apikey.Hash(plaintext), nil
	}
	return "", "", nil
}

func (h *Handler) writeConsumerKey(ctx context.Context, name, keyHash string) error {
	if keyHash == "" || h.vault == nil {
		return nil
	}
	vaultPath := h.opts.VaultPrefix + pathConsumers + name
	existing, version, err := h.vault.ReadVersion(ctx, h.opts.VaultMount, vaultPath)
	if err != nil && !errors.Is(err, vault.ErrNotFound) {
		return fmt.Errorf("vault read version: %w", err)
	}
	data := make(map[string]any)
	for k, v := range existing {
		data[k] = v
	}
	data["key_sha256"] = keyHash
	delete(data, "issued_at")
	delete(data, "superseded_sha256")
	delete(data, "unwritten_sha256")
	return h.vault.Write(ctx, h.opts.VaultMount, vaultPath, data, version)
}

func (h *Handler) checkKeyHashUnique(ctx context.Context, keyHash, excludeConsumer string) (string, error) {
	if h.vault == nil || keyHash == "" {
		return "", nil
	}
	prefix := strings.TrimSuffix(h.opts.VaultPrefix+pathConsumers, "/")
	keys, err := h.vault.List(ctx, h.opts.VaultMount, prefix)
	if err != nil {
		if errors.Is(err, vault.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("list vault consumers: %w", err)
	}
	for _, k := range keys {
		otherName := strings.TrimSuffix(k, "/")
		if otherName == "" || otherName == excludeConsumer {
			continue
		}
		data, err := h.vault.Read(ctx, h.opts.VaultMount, h.opts.VaultPrefix+pathConsumers+otherName)
		if err != nil {
			continue
		}
		for _, hsh := range apikey.SplitHashes(fmt.Sprint(data["key_sha256"])) {
			if hsh == keyHash {
				return otherName, nil
			}
		}
	}
	return "", nil
}

func (h *Handler) createConsumer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req CreateConsumerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := validateCreateConsumer(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)

	plaintext, keyHash, err := resolveConsumerKey(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.CustomKey != "" && keyHash != "" && h.vault != nil {
		existingUser, err := h.checkKeyHashUnique(ctx, keyHash, name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "check key hash: "+err.Error())
			return
		}
		if existingUser != "" {
			writeError(w, http.StatusConflict, fmt.Sprintf("key hash is already in use by consumer %q", existingUser))
			return
		}
	}

	declBytes, err := encodeConsumerDeclaration(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode declaration: "+err.Error())
		return
	}
	consulKey := h.opts.ConsulPrefix + pathConsumers + name
	if err := h.consul.PutCAS(ctx, consulKey, declBytes, 0); err != nil {
		if errors.Is(err, consul.ErrConflict) {
			writeError(w, http.StatusConflict, "consumer already exists")
			return
		}
		h.log.Error("consul put consumer failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, "consul put: "+err.Error())
		return
	}

	hasKey := false
	if keyHash != "" && h.vault != nil {
		if err := h.writeConsumerKey(ctx, name, keyHash); err != nil {
			_ = h.consul.Delete(ctx, consulKey, false)
			if errors.Is(err, vault.ErrConflict) {
				writeError(w, http.StatusConflict, "vault check-and-set conflict")
				return
			}
			h.log.Error("vault write consumer key failed", "consumer", name, "error", err)
			writeError(w, http.StatusInternalServerError, "vault write key: "+err.Error())
			return
		}
		hasKey = true
	}

	resp := CreateConsumerResponse{
		ConsumerItem: ConsumerItem{
			Name:         name,
			QuotaProfile: req.QuotaProfile,
			Namespace:    req.Namespace,
			Secret:       req.Secret,
			HasKey:       hasKey,
			Status:       "ready",
		},
		PlaintextKey: plaintext,
	}
	if plaintext != "" {
		resp.Message = "Consumer created. Save this API key now; it cannot be shown again."
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (h *Handler) deleteConsumer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, errConsumerNameRequired)
		return
	}
	if !isValidName(name) {
		writeError(w, http.StatusBadRequest, errInvalidConsumerName)
		return
	}

	// Check consumer existence in Consul
	consulKey := h.opts.ConsulPrefix + pathConsumers + name
	pairs, _, err := h.consul.List(ctx, consulKey, 0, 0)
	if err != nil {
		h.log.Error("consul list consumer failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, errConsulListPrefix+err.Error())
		return
	}
	if findExactKVPair(pairs, consulKey) == nil {
		writeError(w, http.StatusNotFound, "consumer not found")
		return
	}

	// 1. Delete from Vault first: if denied by policy or down, do not delete from Consul
	if h.vault != nil {
		vaultPath := h.opts.VaultPrefix + pathConsumers + name
		if err := h.vault.Delete(ctx, h.opts.VaultMount, vaultPath); err != nil && !errors.Is(err, vault.ErrNotFound) {
			h.log.Error("vault delete consumer key failed", "consumer", name, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to delete consumer in vault: "+err.Error())
			return
		}
	}

	// 2. Delete declaration from Consul
	if err := h.consul.Delete(ctx, consulKey, false); err != nil {
		h.log.Error("consul delete consumer failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, "consul delete: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "name": name})
}

func (h *Handler) rotateConsumerKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, errConsumerNameRequired)
		return
	}
	if !isValidName(name) {
		writeError(w, http.StatusBadRequest, errInvalidConsumerName)
		return
	}
	if h.vault == nil {
		writeError(w, http.StatusInternalServerError, "vault client is not configured")
		return
	}

	// Check declaration in Consul: refuse for Kubernetes-managed consumers
	consulKey := h.opts.ConsulPrefix + pathConsumers + name
	pairs, _, err := h.consul.List(ctx, consulKey, 0, 0)
	if err != nil {
		h.log.Error("consul list consumer failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, errConsulListPrefix+err.Error())
		return
	}
	pair := findExactKVPair(pairs, consulKey)
	if pair == nil {
		writeError(w, http.StatusNotFound, "consumer not found")
		return
	}
	var decl struct {
		Namespace string `json:"namespace"`
	}
	_ = json.Unmarshal(pair.Value, &decl)
	if decl.Namespace != "" {
		writeError(w, http.StatusBadRequest, "cannot rotate key for Kubernetes-managed consumer; key rotation is handled by Kubernetes Secret reconciliation")
		return
	}

	// Read existing Vault record with check-and-set version
	vaultPath := h.opts.VaultPrefix + pathConsumers + name
	data, version, err := h.vault.ReadVersion(ctx, h.opts.VaultMount, vaultPath)
	if errors.Is(err, vault.ErrNotFound) {
		version = 0
		data = map[string]any{}
	} else if err != nil {
		h.log.Error("vault read version failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, "vault read version: "+err.Error())
		return
	}

	plaintext, err := apikey.New(nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate key: "+err.Error())
		return
	}
	newHash := apikey.Hash(plaintext)

	// Standalone consumers have no Kubernetes Secret reconciler to clean grace periods.
	// Replace key_sha256 immediately while preserving all unknown fields.
	newData := make(map[string]any)
	for k, v := range data {
		newData[k] = v
	}
	newData["key_sha256"] = newHash
	delete(newData, "issued_at")
	delete(newData, "superseded_sha256")
	delete(newData, "unwritten_sha256")

	if err := h.vault.Write(ctx, h.opts.VaultMount, vaultPath, newData, version); err != nil {
		if errors.Is(err, vault.ErrConflict) {
			writeError(w, http.StatusConflict, "vault check-and-set conflict")
			return
		}
		h.log.Error("vault rotate key failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, "vault rotate key: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"name":          name,
		"plaintext_key": plaintext,
		"message":       "Key rotated successfully. Save this API key now; it cannot be shown again.",
	})
}

// QuotaItem represents a quota profile in Consul KV.
type QuotaItem struct {
	Name          string `json:"name"`
	TokensPerHour int64  `json:"tokens_per_hour"`
	Status        string `json:"status,omitempty"` // ready, invalid
}

func (h *Handler) fetchQuotas(ctx context.Context) ([]QuotaItem, error) {
	prefix := h.opts.ConsulPrefix + pathQuotas
	pairs, _, err := h.consul.List(ctx, prefix, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("list quotas: %w", err)
	}
	items := make([]QuotaItem, 0, len(pairs))
	for _, p := range pairs {
		name := strings.TrimPrefix(p.Key, prefix)
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		var q struct {
			TokensPerHour int64 `json:"tokens_per_hour"`
		}
		if err := json.Unmarshal(p.Value, &q); err != nil {
			items = append(items, QuotaItem{Name: name, Status: "invalid"})
			continue
		}
		items = append(items, QuotaItem{Name: name, TokensPerHour: q.TokensPerHour, Status: "ready"})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

func (h *Handler) listQuotas(w http.ResponseWriter, r *http.Request) {
	items, err := h.fetchQuotas(r.Context())
	if err != nil {
		h.log.Error("list quotas failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list quotas: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *Handler) createQuota(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req QuotaItem
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, errQuotaNameRequired)
		return
	}
	if !isValidName(name) {
		writeError(w, http.StatusBadRequest, "invalid quota profile name")
		return
	}
	if req.TokensPerHour < 0 {
		writeError(w, http.StatusBadRequest, "tokens_per_hour must be non-negative")
		return
	}

	payload, _ := json.Marshal(map[string]int64{"tokens_per_hour": req.TokensPerHour})
	key := h.opts.ConsulPrefix + pathQuotas + name
	if err := h.consul.PutCAS(ctx, key, payload, 0); err != nil {
		if errors.Is(err, consul.ErrConflict) {
			writeError(w, http.StatusConflict, "quota profile already exists")
			return
		}
		h.log.Error("consul put quota failed", "profile", name, "error", err)
		writeError(w, http.StatusInternalServerError, "consul put: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, QuotaItem{Name: name, TokensPerHour: req.TokensPerHour, Status: "ready"})
}

func (h *Handler) deleteQuota(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, errQuotaNameRequired)
		return
	}
	if !isValidName(name) {
		writeError(w, http.StatusBadRequest, "invalid quota profile name")
		return
	}
	key := h.opts.ConsulPrefix + pathQuotas + name
	pairs, _, err := h.consul.List(ctx, key, 0, 0)
	if err != nil {
		h.log.Error("consul list quota failed", "profile", name, "error", err)
		writeError(w, http.StatusInternalServerError, errConsulListPrefix+err.Error())
		return
	}
	if findExactKVPair(pairs, key) == nil {
		writeError(w, http.StatusNotFound, "quota profile not found")
		return
	}
	if err := h.consul.Delete(ctx, key, false); err != nil {
		h.log.Error("consul delete quota failed", "profile", name, "error", err)
		writeError(w, http.StatusInternalServerError, "consul delete: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "name": name})
}

// BackendItem is a registered instance of aisa-backend in Consul.
type BackendItem struct {
	ID         string   `json:"id"`
	Service    string   `json:"service"`
	Address    string   `json:"address"`
	Port       int      `json:"port"`
	Tags       []string `json:"tags"`
	Provider   string   `json:"provider"`
	Models     []string `json:"models"`
	Priority   string   `json:"priority"`
	FailPolicy string   `json:"fail_policy"`
	Timeout    string   `json:"timeout"`
	Status     string   `json:"status"` // passing, warning, critical
	Output     string   `json:"output,omitempty"`
}

func backendStatus(inst consul.Instance) (string, string) {
	if inst.Passing {
		return "passing", ""
	}
	status := "critical"
	output := ""
	for _, chk := range inst.Checks {
		if chk.Status != "passing" {
			status = chk.Status
			output = chk.Output
			break
		}
	}
	return status, output
}

func parseBackendModels(meta map[string]string) []string {
	rawModels := meta["models"]
	if rawModels == "" {
		return []string{}
	}
	models := []string{}
	for _, m := range strings.Split(rawModels, ",") {
		if trimmed := strings.TrimSpace(m); trimmed != "" {
			models = append(models, trimmed)
		}
	}
	return models
}

func (h *Handler) fetchBackends(ctx context.Context) ([]BackendItem, error) {
	instances, err := h.consul.Health(ctx, "aisa-backend")
	if err != nil {
		return nil, fmt.Errorf("consul health: %w", err)
	}
	backends := make([]BackendItem, 0, len(instances))
	for _, inst := range instances {
		status, output := backendStatus(inst)
		backends = append(backends, BackendItem{
			ID:         inst.ID,
			Service:    inst.Service,
			Address:    inst.Address,
			Port:       inst.Port,
			Tags:       inst.Tags,
			Provider:   inst.Meta["provider"],
			Models:     parseBackendModels(inst.Meta),
			Priority:   inst.Meta["priority"],
			FailPolicy: inst.Meta["fail_policy"],
			Timeout:    inst.Meta["timeout"],
			Status:     status,
			Output:     output,
		})
	}
	sort.Slice(backends, func(i, j int) bool { return backends[i].ID < backends[j].ID })
	return backends, nil
}

func (h *Handler) listBackends(w http.ResponseWriter, r *http.Request) {
	items, err := h.fetchBackends(r.Context())
	if err != nil {
		h.log.Error("list backends failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list backends: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// ModelItem aggregates a model across available backends.
type ModelItem struct {
	Name     string   `json:"name"`
	Backends []string `json:"backends"`
	Healthy  bool     `json:"healthy"`
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	backends, err := h.fetchBackends(r.Context())
	if err != nil {
		h.log.Error("list models failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list models: "+err.Error())
		return
	}
	modelMap := make(map[string]*ModelItem)
	for _, b := range backends {
		for _, m := range b.Models {
			item, exists := modelMap[m]
			if !exists {
				item = &ModelItem{Name: m, Backends: []string{}}
				modelMap[m] = item
			}
			item.Backends = append(item.Backends, b.ID)
			if b.Status == "passing" {
				item.Healthy = true
			}
		}
	}
	models := make([]ModelItem, 0, len(modelMap))
	for _, m := range modelMap {
		models = append(models, *m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	writeJSON(w, http.StatusOK, models)
}
