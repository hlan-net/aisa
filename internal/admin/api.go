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

var validNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func isValidName(name string) bool {
	return len(name) >= 1 && len(name) <= 253 && validNameRE.MatchString(name) && name != metrics.ConsumerUnknown
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

func probeWithin(ctx context.Context, c server.Check) error {
	done := make(chan error, 1)
	go func() { done <- c.Probe(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	errs := make([]error, len(h.checks))
	var wg sync.WaitGroup
	for i, c := range h.checks {
		wg.Go(func() { errs[i] = probeWithin(ctx, c) })
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
	errQuotaNameRequired    = "quota profile name is required"
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
		name := strings.TrimPrefix(p.Key, prefix)
		if name == "" || strings.HasSuffix(name, "/") {
			continue
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
		items = append(items, ConsumerItem{
			Name:         name,
			QuotaProfile: decl.QuotaProfile,
			Namespace:    decl.Namespace,
			Secret:       decl.Secret,
			HasKey:       hasKey,
			Status:       status,
		})
	}

	// Also check for undeclared keys in Vault
	consulNames := make(map[string]bool)
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

func (h *Handler) createConsumer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req CreateConsumerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, errConsumerNameRequired)
		return
	}
	if !isValidName(name) {
		writeError(w, http.StatusBadRequest, "invalid consumer name")
		return
	}
	if (req.Namespace == "") != (req.Secret == "") {
		writeError(w, http.StatusBadRequest, "namespace and secret must be specified together")
		return
	}
	if req.Namespace != "" && (req.GenerateKey || req.CustomKey != "") {
		writeError(w, http.StatusBadRequest, "keys for Kubernetes-managed consumers (with namespace/secret) are managed by Kubernetes Secrets; manual key issuance is not permitted")
		return
	}
	if req.CustomKey != "" && len(strings.TrimSpace(req.CustomKey)) < apikey.MinCustomKeyLength {
		writeError(w, http.StatusBadRequest, apikey.ErrKeyTooShort.Error())
		return
	}

	// 1. Write declaration to Consul with CAS=0 (only create if not exists)
	decl := map[string]string{}
	if req.QuotaProfile != "" {
		decl["quota_profile"] = strings.TrimSpace(req.QuotaProfile)
	}
	if req.Namespace != "" && req.Secret != "" {
		decl["namespace"] = strings.TrimSpace(req.Namespace)
		decl["secret"] = strings.TrimSpace(req.Secret)
	}
	declBytes, err := json.Marshal(decl)
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

	// 2. Handle API key if requested for standalone consumer
	var plaintext, keyHash string
	if req.GenerateKey {
		var err error
		plaintext, err = apikey.New(nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "generate key: "+err.Error())
			return
		}
		keyHash = apikey.Hash(plaintext)
	} else if req.CustomKey != "" {
		plaintext = strings.TrimSpace(req.CustomKey)
		keyHash = apikey.Hash(plaintext)
	}

	hasKey := false
	if keyHash != "" && h.vault != nil {
		vaultPath := h.opts.VaultPrefix + pathConsumers + name
		_, version, err := h.vault.ReadVersion(ctx, h.opts.VaultMount, vaultPath)
		if err != nil && !errors.Is(err, vault.ErrNotFound) {
			h.log.Error("vault read version failed", "consumer", name, "error", err)
			writeError(w, http.StatusInternalServerError, "vault read version: "+err.Error())
			return
		}
		data := map[string]any{
			"key_sha256": keyHash,
			"issued_at":  time.Now().UTC().Format(time.RFC3339),
		}
		if err := h.vault.Write(ctx, h.opts.VaultMount, vaultPath, data, version); err != nil {
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
		writeError(w, http.StatusBadRequest, "invalid consumer name")
		return
	}

	// Check consumer existence in Consul
	consulKey := h.opts.ConsulPrefix + pathConsumers + name
	pairs, _, err := h.consul.List(ctx, consulKey, 0, 0)
	if err != nil {
		h.log.Error("consul list consumer failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, "consul list: "+err.Error())
		return
	}
	if len(pairs) == 0 {
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
		writeError(w, http.StatusBadRequest, "invalid consumer name")
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
		writeError(w, http.StatusInternalServerError, "consul list: "+err.Error())
		return
	}
	if len(pairs) == 0 {
		writeError(w, http.StatusNotFound, "consumer not found")
		return
	}
	var decl struct {
		Namespace string `json:"namespace"`
	}
	_ = json.Unmarshal(pairs[0].Value, &decl)
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
	now := time.Now().UTC().Format(time.RFC3339)

	// Preserve old hashes in superseded_sha256 during grace period
	oldHashes := apikey.SplitHashes(fmt.Sprint(data["key_sha256"]))
	superseded := make(map[string]any)
	if sm, ok := data["superseded_sha256"].(map[string]any); ok {
		for k, v := range sm {
			superseded[k] = v
		}
	}
	for _, oh := range oldHashes {
		if _, ok := superseded[oh]; !ok {
			superseded[oh] = now
		}
	}
	allHashes := append(oldHashes, newHash)
	newData := map[string]any{
		"key_sha256":        strings.Join(allHashes, ","),
		"superseded_sha256": superseded,
		"issued_at":         now,
	}

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
		if err := json.Unmarshal(p.Value, &q); err == nil {
			items = append(items, QuotaItem{Name: name, TokensPerHour: q.TokensPerHour})
		}
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
	if req.TokensPerHour <= 0 {
		writeError(w, http.StatusBadRequest, "tokens_per_hour must be positive")
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

	writeJSON(w, http.StatusCreated, QuotaItem{Name: name, TokensPerHour: req.TokensPerHour})
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
		writeError(w, http.StatusInternalServerError, "consul list: "+err.Error())
		return
	}
	if len(pairs) == 0 {
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

func (h *Handler) fetchBackends(ctx context.Context) ([]BackendItem, error) {
	instances, err := h.consul.Health(ctx, "aisa-backend")
	if err != nil {
		return nil, fmt.Errorf("consul health: %w", err)
	}
	backends := make([]BackendItem, 0, len(instances))
	for _, inst := range instances {
		status := "passing"
		output := ""
		if !inst.Passing {
			status = "critical"
			for _, chk := range inst.Checks {
				if chk.Status != "passing" {
					status = chk.Status
					output = chk.Output
					break
				}
			}
		}

		models := []string{}
		if rawModels := inst.Meta["models"]; rawModels != "" {
			for _, m := range strings.Split(rawModels, ",") {
				if trimmed := strings.TrimSpace(m); trimmed != "" {
					models = append(models, trimmed)
				}
			}
		}

		backends = append(backends, BackendItem{
			ID:         inst.ID,
			Service:    inst.Service,
			Address:    inst.Address,
			Port:       inst.Port,
			Tags:       inst.Tags,
			Provider:   inst.Meta["provider"],
			Models:     models,
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
