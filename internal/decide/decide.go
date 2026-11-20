// Package decide is the decision API, contract 1 in docs/concepts/PROXY_CONTRACT.md: the
// gateway asks POST /v1/decide before it proxies a request, and applies the answer.
//
// It authenticates the consumer, denies a consumer without a quota profile, checks its token
// quota and its monthly budget, and answers with the model the request is to be sent to: the
// requested one, or the budget's downgrade when the budget is used up.
package decide

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hlan-net/aisa/internal/budgets"
	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/metrics"
	"github.com/hlan-net/aisa/internal/models"
	"github.com/hlan-net/aisa/internal/quotas"
)

// Headers of the contract.
const (
	HeaderRequestedModel  = "X-Aisa-Requested-Model"
	HeaderConsumer        = "X-Aisa-Consumer"
	HeaderModel           = "X-Aisa-Model"
	HeaderBudgetRemaining = "X-Aisa-Budget-Remaining"
	HeaderQuotaRemaining  = "X-Aisa-Quota-Remaining"
	HeaderQuotaReset      = "X-Aisa-Quota-Reset"
	HeaderRequestID       = "X-Request-Id"
)

// maxBody bounds the request body aisa reads to find the model. The gateway may forward the
// whole client request, prompt and images included. A proxy that sets
// X-Aisa-Requested-Model spares aisa the body.
const maxBody = 16 << 20

// maxModel is the longest model name aisa accepts, in bytes, from the header and the body alike.
const maxModel = 256

// Lookup finds the consumer of a key; consumers.Store implements it.
type Lookup interface {
	Lookup(ctx context.Context, key string) (consumers.Consumer, consumers.Result)
}

// QuotaChecker checks a consumer's token quota; quotas.Quota implements it.
type QuotaChecker interface {
	Check(ctx context.Context, c consumers.Consumer) quotas.Verdict
}

// BudgetChecker checks a consumer's monthly budget for a model; budgets.Monthly implements it.
type BudgetChecker interface {
	Check(ctx context.Context, consumer, model string) budgets.Verdict
}

// Resolver places a requested model name: a model's, an alias of one, or unknown;
// models.Registry implements it.
type Resolver interface {
	Resolve(name string) (string, models.Outcome)
}

// ProviderChecker checks the budgets of the provider keys that serve a model, the ceiling
// above the consumers; budgets.Ceiling implements it.
type ProviderChecker interface {
	Check(ctx context.Context, model string) budgets.Verdict
}

// Options configure the decision handler.
type Options struct {
	// DefaultQuotaProfile is the quota profile of a consumer that names none
	// (AISA_DEFAULT_QUOTA_PROFILE). Empty: such a consumer is denied with 403 no_quota, so no
	// consumer is served without a limit the holder set (#33).
	DefaultQuotaProfile string
	// Budget checks the consumer's monthly budget. Nil: budgets are not checked.
	Budget BudgetChecker
	// Provider checks the budgets of the provider keys that serve the model. Nil: none.
	Provider ProviderChecker
	// Models resolves aliases to their models and tells a name aisa does not know (#89). Nil:
	// the requested name is taken as it is.
	Models Resolver
}

// Handler answers /v1/decide.
type Handler struct {
	consumers Lookup
	quota     QuotaChecker
	budget    BudgetChecker
	provider  ProviderChecker
	metrics   *metrics.Metrics
	log       *slog.Logger
	opts      Options
}

// New returns the decision handler. With a nil quota, quotas are not checked; a consumer still
// needs a quota profile, its own or the default.
func New(c Lookup, q QuotaChecker, m *metrics.Metrics, log *slog.Logger, opts Options) *Handler {
	return &Handler{consumers: c, quota: q, budget: opts.Budget, provider: opts.Provider, metrics: m, log: log, opts: opts}
}

// ServeHTTP answers POST and GET /v1/decide.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	result := h.decide(w, r)
	h.metrics.DecideDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
}

// decide answers the request and returns the result label of aisa_decisions_total it counted.
// The stages run in the order of the contract: the credential, the model, the quota, the
// consumer's budget, the provider's budget. Each returns its denial, or nil to go on.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request) string {
	reqID := r.Header.Get(HeaderRequestID)
	log := h.log.With("request_id", reqID)

	consumer, d := h.authenticate(r, log)
	if d != nil {
		return h.deny(w, log, metrics.ConsumerUnknown, *d)
	}
	model, d := h.model(r)
	if d != nil {
		return h.deny(w, log, consumer.Name, *d)
	}
	if consumer.QuotaProfile == "" {
		if h.opts.DefaultQuotaProfile == "" {
			return h.deny(w, log, consumer.Name, denial{
				status: http.StatusForbidden, result: metrics.ResultNoQuota,
				typ: "invalid_request_error", code: "no_quota",
				msg: "the consumer has no quota profile, and aisa serves no consumer without a limit",
			})
		}
		consumer.QuotaProfile = h.opts.DefaultQuotaProfile
	}
	qh, quotaDenial, denied := h.checkQuota(r.Context(), log, consumer)
	if denied {
		return h.deny(w, log, consumer.Name, quotaDenial)
	}
	rt := route{model: model, result: metrics.ResultAllow}
	// remaining is X-Aisa-Budget-Remaining, sent with an allowed request of a consumer that has
	// a budget; a denial carries no header of the kind.
	remaining, d := h.checkBudget(r.Context(), log, consumer.Name, &rt)
	if d != nil {
		return h.deny(w, log, consumer.Name, *d)
	}
	if d := h.checkProvider(r.Context(), log, consumer.Name, &rt); d != nil {
		return h.deny(w, log, consumer.Name, *d)
	}

	w.Header().Set(HeaderConsumer, consumer.Name)
	w.Header().Set(HeaderModel, rt.model)
	if remaining != "" {
		w.Header().Set(HeaderBudgetRemaining, remaining)
	}
	if qh.remaining != "" {
		w.Header().Set(HeaderQuotaRemaining, qh.remaining)
		w.Header().Set(HeaderQuotaReset, qh.reset)
	}
	w.WriteHeader(http.StatusOK)
	h.metrics.Decisions.WithLabelValues(consumer.Name, rt.result).Inc()
	if rt.result == metrics.ResultAllow {
		log.Debug("allow", "consumer", consumer.Name, "model", rt.model)
	}
	return rt.result
}

// route is where an allowed request goes: the model, which a downgrade changes, and the result
// it is counted under, allow or downgrade.
type route struct {
	model, result string
}

// downgrade sends the request to another model; the proxy routes by X-Aisa-Model and forwards
// the request with that model (proxy rule 3).
func (rt *route) downgrade(h *Handler, to string) {
	rt.model, _ = h.resolve(to)
	rt.result = metrics.ResultDowngrade
}

// authenticate returns the consumer of the request's key, or the denial of a request without
// one that aisa knows.
func (h *Handler) authenticate(r *http.Request, log *slog.Logger) (consumers.Consumer, *denial) {
	key, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		d := denyAuth("missing or malformed credential: want Authorization: Bearer <key>")
		return consumers.Consumer{}, &d
	}
	stage := time.Now()
	consumer, res := h.consumers.Lookup(r.Context(), key)
	h.metrics.DecideStageDuration.WithLabelValues(metrics.StageConsumer).Observe(time.Since(stage).Seconds())
	switch res {
	case consumers.Unavailable:
		// Fail closed: without current consumers aisa cannot tell a valid key from a revoked one.
		log.Error("cannot decide: consumers are not available")
		return consumers.Consumer{}, &denial{
			status: http.StatusServiceUnavailable, result: metrics.ResultUnavailable,
			typ: "server_error", code: "consumers_unavailable", msg: "aisa cannot verify credentials at the moment",
		}
	case consumers.Unknown:
		d := denyAuth("unknown or invalid consumer key")
		return consumers.Consumer{}, &d
	}
	return consumer, nil
}

// model returns the model the request asks for, an alias resolved to its model, or the denial
// of a request whose model cannot be read or is not one aisa knows.
func (h *Handler) model(r *http.Request) (string, *denial) {
	model, err := requestedModel(r)
	if errors.Is(err, errBodyTooLarge) {
		return "", &denial{
			status: http.StatusRequestEntityTooLarge, result: metrics.ResultInvalid,
			typ: "invalid_request_error", code: "request_too_large",
			msg: "the request is too large for aisa to find its model; the gateway should send the model in X-Aisa-Requested-Model",
		}
	}
	if errors.Is(err, errModelTooLong) {
		return "", &denial{
			status: http.StatusBadRequest, result: metrics.ResultInvalid,
			typ: "invalid_request_error", code: "invalid_model",
			msg: fmt.Sprintf("the requested model name is longer than %d bytes", maxModel),
		}
	}
	if model == "" {
		return "", &denial{
			status: http.StatusBadRequest, result: metrics.ResultInvalid,
			typ: "invalid_request_error", code: "missing_model", msg: "the requested model could not be determined",
		}
	}
	model, known := h.resolve(model)
	if !known {
		return "", &denial{
			status: http.StatusNotFound, result: metrics.ResultUnknownModel,
			typ: "invalid_request_error", code: "model_not_found",
			msg: fmt.Sprintf("the model %q does not exist; GET /v1/models lists the models on offer", model),
		}
	}
	return model, nil
}

// budgetUnavailable is the answer when a budget cannot be checked. Fail closed: a broken
// budget must not lift the limit unnoticed.
func budgetUnavailable() *denial {
	return &denial{
		status: http.StatusServiceUnavailable, result: metrics.ResultUnavailable,
		typ: "server_error", code: "budget_unavailable", msg: "aisa cannot check the monthly budget at the moment",
	}
}

// checkBudget applies the consumer's monthly budget: it returns the denial of a consumer
// whose budget is used up or cannot be known, or sends the request to the budget's downgrade
// model. The string is the consumer's remaining tokens, "" for a consumer without a budget.
func (h *Handler) checkBudget(ctx context.Context, log *slog.Logger, consumer string, rt *route) (string, *denial) {
	if h.budget == nil {
		return "", nil
	}
	v := h.budget.Check(ctx, consumer, rt.model)
	switch v.Outcome {
	case budgets.Unavailable:
		log.Error("cannot check the budget", "consumer", consumer, "error", v.Err)
		return "", budgetUnavailable()
	case budgets.Exhausted:
		if v.OnExhausted != budgets.Downgrade || v.DowngradeTo == "" || v.DowngradeTo == rt.model {
			return "", &denial{
				status: http.StatusTooManyRequests, result: metrics.ResultDenyBudget,
				typ: "insufficient_quota", code: "budget_exhausted",
				msg: fmt.Sprintf("monthly budget exhausted: %d of %d tokens used on priced models", v.Used, v.Limit),
			}
		}
		// The request goes on, to the budget's downgrade model instead.
		log.Info("downgrade", "consumer", consumer, "model", rt.model, "to", v.DowngradeTo)
		rt.downgrade(h, v.DowngradeTo)
	}
	if v.Outcome != budgets.NoBudget && v.Err == nil {
		return strconv.FormatInt(v.Remaining, 10), nil
	}
	return "", nil
}

// checkProvider applies the ceiling above the consumers (#33): the budgets of the provider
// keys that serve the model, the model the request goes to after a downgrade included. A
// downgrade here goes to the provider budget's model, and the consumer's remaining tokens stay
// as said: the holder's total is not the client's business.
func (h *Handler) checkProvider(ctx context.Context, log *slog.Logger, consumer string, rt *route) *denial {
	if h.provider == nil {
		return nil
	}
	// Twice at most: the downgrade's model is checked too, and is not downgraded again.
	for downgraded := false; ; downgraded = true {
		v := h.provider.Check(ctx, rt.model)
		if v.Outcome == budgets.Unavailable {
			log.Error("cannot check the provider budget", "model", rt.model, "provider", v.Provider, "error", v.Err)
			return budgetUnavailable()
		}
		if v.Outcome != budgets.Exhausted {
			return nil
		}
		if downgraded || v.OnExhausted != budgets.Downgrade || v.DowngradeTo == "" || v.DowngradeTo == rt.model {
			return &denial{
				status: http.StatusTooManyRequests, result: metrics.ResultDenyProviderBudget,
				typ: "insufficient_quota", code: "provider_budget_exhausted",
				msg: fmt.Sprintf("the monthly budget of the provider account that serves %s is exhausted: %d of %d tokens used", rt.model, v.Used, v.Limit),
			}
		}
		log.Info("downgrade", "consumer", consumer, "model", rt.model, "to", v.DowngradeTo, "provider", v.Provider)
		rt.downgrade(h, v.DowngradeTo)
	}
}

// resolve returns the model a name stands for, itself or the model of an alias, and false for a
// name that is neither. A name that cannot be placed yet, before the catalog has been read, is
// let through as it is: the proxy has no route for a name that is no model, and answers 503.
func (h *Handler) resolve(name string) (string, bool) {
	if h.opts.Models == nil {
		return name, true
	}
	model, outcome := h.opts.Models.Resolve(name)
	return model, outcome != models.Unknown
}

type quotaHeaders struct {
	remaining, reset string
}

// checkQuota returns the quota headers on allow, or the denial of a consumer whose quota is
// exhausted or cannot be known.
func (h *Handler) checkQuota(ctx context.Context, log *slog.Logger, c consumers.Consumer) (quotaHeaders, denial, bool) {
	if h.quota == nil {
		return quotaHeaders{}, denial{}, false
	}
	start := time.Now()
	v := h.quota.Check(ctx, c)
	h.metrics.DecideStageDuration.WithLabelValues(metrics.StageQuota).Observe(time.Since(start).Seconds())
	switch v.Outcome {
	case quotas.Exhausted:
		return quotaHeaders{}, denial{
			status: http.StatusTooManyRequests, result: metrics.ResultDenyQuota,
			typ: "tokens", code: "rate_limit_exceeded",
			msg:        fmt.Sprintf("token quota exhausted: %d of %d tokens per hour used", v.Used, v.Limit),
			retryAfter: v.RetryAfter,
		}, true
	case quotas.Unavailable:
		// Fail closed: a missing or broken profile must not lift the limit unnoticed.
		log.Error("cannot check the quota", "consumer", c.Name, "profile", c.QuotaProfile, "error", v.Err)
		return quotaHeaders{}, denial{
			status: http.StatusServiceUnavailable, result: metrics.ResultUnavailable,
			typ: "server_error", code: "quota_unavailable", msg: "aisa cannot check the token quota at the moment",
		}, true
	}
	if v.Err != nil || c.QuotaProfile == "" {
		return quotaHeaders{}, denial{}, false
	}
	rem := max(v.Limit-v.Used, 0)
	reset := max(int64(math.Ceil(v.ResetAfter.Seconds())), 0)
	return quotaHeaders{
		remaining: strconv.FormatInt(rem, 10),
		reset:     strconv.FormatInt(reset, 10),
	}, denial{}, false
}

// denial is a negative answer: its status, its aisa_decisions_total result and its error body.
type denial struct {
	status         int
	result         string
	typ, code, msg string
	// retryAfter is sent as Retry-After when set.
	retryAfter time.Duration
}

func denyAuth(msg string) denial {
	return denial{
		status: http.StatusUnauthorized, result: metrics.ResultDenyAuth,
		typ: "invalid_request_error", code: "invalid_api_key", msg: msg,
	}
}

// deny writes the denial and returns its result label.
func (h *Handler) deny(w http.ResponseWriter, log *slog.Logger, consumer string, d denial) string {
	h.metrics.Decisions.WithLabelValues(consumer, d.result).Inc()
	log.Info("deny", "status", d.status, "consumer", consumer, "result", d.result, "reason", d.code)
	if d.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(d.retryAfter.Seconds())), 10))
	}
	writeError(w, d.status, d.typ, d.code, d.msg)
	return d.result
}

// bearer extracts the key from an Authorization header. The scheme is case-insensitive.
func bearer(v string) (string, bool) {
	scheme, key, ok := strings.Cut(strings.TrimSpace(v), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	key = strings.TrimSpace(key)
	return key, key != ""
}

// errBodyTooLarge: the body is larger than maxBody, so the model in it may not have been seen.
var errBodyTooLarge = errors.New("request body too large")

// errModelTooLong: the model name is longer than maxModel.
var errModelTooLong = fmt.Errorf("model name longer than %d bytes", maxModel)

// requestedModel reads the model from X-Aisa-Requested-Model or, when absent, from the JSON
// request body. The header takes precedence: the proxy sets it from the body and overwrites
// any client value (proxy rule 2). A request without a model, or with a body that is not a
// JSON object, has the model "". A model longer than maxModel is errModelTooLong, from either
// source, so a proxy's choice between the header and the body does not change the answer.
func requestedModel(r *http.Request) (string, error) {
	if m := strings.TrimSpace(r.Header.Get(HeaderRequestedModel)); m != "" {
		if len(m) > maxModel {
			return "", errModelTooLong
		}
		return m, nil
	}
	if r.Body == nil {
		return "", nil
	}
	body := &limited{r: r.Body, left: maxBody}
	model, err := modelOf(body)
	// The scan stops at the end of the object or at an error. Whatever follows still counts
	// towards the limit, or a body of any size would pass with a short object at its start.
	// It is discarded, not kept.
	_, _ = io.Copy(io.Discard, body)
	if body.exceeded {
		return "", errBodyTooLarge
	}
	if errors.Is(err, errModelTooLong) {
		return "", err
	}
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(model), nil
}

// modelOf returns the string value of the key "model" of a JSON object, the last one when the
// key is repeated, as encoding/json and most other parsers choose. A model that is not a string
// is "". A model longer than maxModel is errModelTooLong.
//
// It reads the object byte by byte and keeps only the keys and the model, so the memory it
// needs does not grow with the body: a gateway may forward megabytes of prompt and images.
// It checks the structure only as far as it must to find its way. A body that is not valid
// JSON may still have a model here; the backend rejects such a request later.
func modelOf(r io.Reader) (string, error) {
	s := scanner{r: bufio.NewReader(r)}
	if err := s.expect('{'); err != nil {
		return "", err
	}
	model, long := "", false
	for first := true; ; first = false {
		c, err := s.next()
		if err != nil {
			return "", err
		}
		if c == '}' {
			return checkedModel(model, long)
		}
		if !first {
			if c != ',' {
				return "", fmt.Errorf("want , or } after a value, got %q", c)
			}
			if c, err = s.next(); err != nil {
				return "", err
			}
		}
		if c != '"' {
			return "", fmt.Errorf("want a key, got %q", c)
		}
		key, err := s.str(true)
		if err != nil {
			return "", err
		}
		if err := s.expect(':'); err != nil {
			return "", err
		}
		if c, err = s.next(); err != nil {
			return "", err
		}
		switch {
		case key != "model":
			err = s.skip(c)
		case c == '"':
			model, err = s.str(true)
			long = s.long
		default:
			model, long = "", false
			err = s.skip(c)
		}
		if err != nil {
			return "", err
		}
	}
}

// checkedModel is the model found, or errModelTooLong when it is longer than maxModel. long says
// that it was too long for the scanner to keep.
func checkedModel(model string, long bool) (string, error) {
	if long || len(strings.TrimSpace(model)) > maxModel {
		return "", errModelTooLong
	}
	return model, nil
}

// maxString is the longest key or model that is kept, as it is written in the body: long enough
// for a model of maxModel bytes with every byte escaped. A longer one is read past and is "".
const maxString = 6*maxModel + 2

type scanner struct {
	r   *bufio.Reader
	buf []byte
	// long: the last string that str was to keep was longer than maxString.
	long bool
}

// next returns the next byte that is not white space.
func (s *scanner) next() (byte, error) {
	for {
		c, err := s.r.ReadByte()
		if err != nil {
			return 0, err
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return c, nil
		}
	}
}

func (s *scanner) expect(want byte) error {
	c, err := s.next()
	if err != nil {
		return err
	}
	if c != want {
		return fmt.Errorf("want %q, got %q", want, c)
	}
	return nil
}

// str reads a string whose opening quote has been read. With keep it returns the string, with
// its escapes resolved.
func (s *scanner) str(keep bool) (string, error) {
	s.buf = append(s.buf[:0], '"')
	s.long = false
	for {
		c, err := s.r.ReadByte()
		if err != nil {
			return "", err
		}
		if keep && len(s.buf) <= maxString {
			s.buf = append(s.buf, c)
		}
		switch c {
		case '"':
			if !keep || len(s.buf) > maxString {
				s.long = keep
				return "", nil
			}
			var v string
			if err := json.Unmarshal(s.buf, &v); err != nil {
				return "", fmt.Errorf("string: %w", err)
			}
			return v, nil
		case '\\':
			// The next byte is escaped, a quote included. The digits of \u are ordinary bytes.
			e, err := s.r.ReadByte()
			if err != nil {
				return "", err
			}
			if keep && len(s.buf) <= maxString {
				s.buf = append(s.buf, e)
			}
		}
	}
}

// skip reads past the value that begins with c.
func (s *scanner) skip(c byte) error {
	switch c {
	case '"':
		_, err := s.str(false)
		return err
	case '{', '[':
		return s.skipNested()
	case '}', ']', ',', ':':
		return fmt.Errorf("want a value, got %q", c)
	}
	// A number, true, false or null: it ends before the next comma or bracket.
	for {
		b, err := s.r.Peek(1)
		if err != nil {
			return err
		}
		switch b[0] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return nil
		}
		_, _ = s.r.Discard(1)
	}
}

// skipNested reads to the end of the object or array whose opening bracket has been read.
func (s *scanner) skipNested() error {
	for depth := 1; depth > 0; {
		c, err := s.r.ReadByte()
		if err != nil {
			return err
		}
		switch c {
		case '"':
			if _, err := s.str(false); err != nil {
				return err
			}
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
	return nil
}

// limited reads at most left bytes and notes when there was more to read.
type limited struct {
	r        io.Reader
	left     int64
	exceeded bool
}

func (l *limited) Read(p []byte) (int, error) {
	if l.left <= 0 {
		// At the limit: one more byte tells a body of exactly this size from a larger one.
		var one [1]byte
		if n, _ := l.r.Read(one[:]); n > 0 {
			l.exceeded = true
		}
		return 0, io.EOF
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}

// writeError writes an OpenAI-style error body, which the gateway passes to the client.
func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": code},
	})
}
