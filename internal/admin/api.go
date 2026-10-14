package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hlan-net/aisa/internal/metrics"
	"github.com/hlan-net/aisa/internal/version"
)

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
	ctx := r.Context()
	ready := true
	checks := make(map[string]string, len(h.checks))
	for _, c := range h.checks {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := c.Probe(checkCtx)
		cancel()
		if err != nil {
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
		if err == nil {
			for _, k := range keys {
				vaultKeys[strings.TrimSuffix(k, "/")] = true
			}
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
	if name == metrics.ConsumerUnknown || strings.ContainsAny(name, " /\\?#") {
		writeError(w, http.StatusBadRequest, "invalid consumer name")
		return
	}
	if (req.Namespace == "") != (req.Secret == "") {
		writeError(w, http.StatusBadRequest, "namespace and secret must be specified together")
		return
	}

	// 1. Write declaration to Consul
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
	if err := h.consul.Put(ctx, consulKey, declBytes); err != nil {
		h.log.Error("consul put consumer failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, "consul put: "+err.Error())
		return
	}

	// 2. Handle API key if requested
	var plaintext, keyHash string
	if req.GenerateKey {
		var err error
		plaintext, keyHash, err = generateAPIKey()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "generate key: "+err.Error())
			return
		}
	} else if req.CustomKey != "" {
		plaintext = strings.TrimSpace(req.CustomKey)
		keyHash = hashKey(plaintext)
	}

	hasKey := false
	if keyHash != "" && h.vault != nil {
		vaultPath := h.opts.VaultPrefix + pathConsumers + name
		err := h.vault.Write(ctx, h.opts.VaultMount, vaultPath, map[string]any{"key_sha256": keyHash})
		if err != nil {
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

	// Delete from Consul
	consulKey := h.opts.ConsulPrefix + pathConsumers + name
	if err := h.consul.Delete(ctx, consulKey, false); err != nil {
		h.log.Error("consul delete consumer failed", "consumer", name, "error", err)
		writeError(w, http.StatusInternalServerError, "consul delete: "+err.Error())
		return
	}

	// Delete from Vault
	if h.vault != nil {
		vaultPath := h.opts.VaultPrefix + pathConsumers + name
		if err := h.vault.Delete(ctx, h.opts.VaultMount, vaultPath); err != nil {
			h.log.Error("vault delete consumer key failed", "consumer", name, "error", err)
		}
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
	if h.vault == nil {
		writeError(w, http.StatusInternalServerError, "vault client is not configured")
		return
	}

	plaintext, keyHash, err := generateAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate key: "+err.Error())
		return
	}

	vaultPath := h.opts.VaultPrefix + pathConsumers + name
	if err := h.vault.Write(ctx, h.opts.VaultMount, vaultPath, map[string]any{"key_sha256": keyHash}); err != nil {
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
	if req.TokensPerHour <= 0 {
		writeError(w, http.StatusBadRequest, "tokens_per_hour must be positive")
		return
	}

	payload, _ := json.Marshal(map[string]int64{"tokens_per_hour": req.TokensPerHour})
	key := h.opts.ConsulPrefix + pathQuotas + name
	if err := h.consul.Put(ctx, key, payload); err != nil {
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
	key := h.opts.ConsulPrefix + pathQuotas + name
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
	sh, err := h.consul.HealthService(ctx, "aisa-backend", false)
	if err != nil {
		return nil, fmt.Errorf("consul health service: %w", err)
	}
	backends := make([]BackendItem, 0, len(sh))
	for _, entry := range sh {
		s := entry.Service
		status := "passing"
		output := ""
		for _, chk := range entry.Checks {
			if chk.Status != "passing" {
				status = chk.Status
				output = chk.Output
				break
			}
		}

		models := []string{}
		if rawModels := s.Meta["models"]; rawModels != "" {
			for _, m := range strings.Split(rawModels, ",") {
				if trimmed := strings.TrimSpace(m); trimmed != "" {
					models = append(models, trimmed)
				}
			}
		}

		backends = append(backends, BackendItem{
			ID:         s.ID,
			Service:    s.Service,
			Address:    s.Address,
			Port:       s.Port,
			Tags:       s.Tags,
			Provider:   s.Meta["provider"],
			Models:     models,
			Priority:   s.Meta["priority"],
			FailPolicy: s.Meta["fail_policy"],
			Timeout:    s.Meta["timeout"],
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
