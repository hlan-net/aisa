// Package decide is the decision API, contract 1 in docs/concepts/ADAPTER_CONTRACT.md: the
// gateway asks POST /v1/decide before it proxies a request, and applies the answer.
//
// This version authenticates the consumer and passes the requested model through. Quotas and
// budgets, which can deny a request or rewrite its model, come in later versions.
package decide

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/metrics"
)

// Headers of the contract.
const (
	HeaderRequestedModel = "X-Aisa-Requested-Model"
	HeaderConsumer       = "X-Aisa-Consumer"
	HeaderModel          = "X-Aisa-Model"
	HeaderRequestID      = "X-Request-Id"
)

// maxBody bounds the request body aisa reads to find the model. The gateway may forward the
// whole client request, prompt and images included. An adapter that sets
// X-Aisa-Requested-Model spares aisa the body.
const maxBody = 16 << 20

// maxModel is the longest model name aisa accepts, in bytes, from the header and the body alike.
const maxModel = 256

// Lookup finds the consumer of a key; consumers.Store implements it.
type Lookup interface {
	Lookup(ctx context.Context, key string) (consumers.Consumer, consumers.Result)
}

// Handler answers /v1/decide.
type Handler struct {
	consumers Lookup
	metrics   *metrics.Metrics
	log       *slog.Logger
}

// New returns the decision handler.
func New(c Lookup, m *metrics.Metrics, log *slog.Logger) *Handler {
	return &Handler{consumers: c, metrics: m, log: log}
}

// ServeHTTP answers POST and GET /v1/decide.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get(HeaderRequestID)
	log := h.log.With("request_id", reqID)

	key, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		h.deny(w, log, metrics.ConsumerUnknown, denyAuth("missing or malformed credential: want Authorization: Bearer <key>"))
		return
	}
	consumer, res := h.consumers.Lookup(r.Context(), key)
	switch res {
	case consumers.Unavailable:
		// Fail closed: without current consumers aisa cannot tell a valid key from a revoked one.
		log.Error("cannot decide: consumers are not available")
		h.deny(w, log, metrics.ConsumerUnknown, denial{
			status: http.StatusServiceUnavailable, result: metrics.ResultUnavailable,
			typ: "server_error", code: "consumers_unavailable", msg: "aisa cannot verify credentials at the moment",
		})
		return
	case consumers.Unknown:
		h.deny(w, log, metrics.ConsumerUnknown, denyAuth("unknown or invalid consumer key"))
		return
	}

	model, err := requestedModel(r)
	if errors.Is(err, errBodyTooLarge) {
		h.deny(w, log, consumer.Name, denial{
			status: http.StatusRequestEntityTooLarge, result: metrics.ResultInvalid,
			typ: "invalid_request_error", code: "request_too_large",
			msg: "the request is too large for aisa to find its model; the gateway should send the model in X-Aisa-Requested-Model",
		})
		return
	}
	if errors.Is(err, errModelTooLong) {
		h.deny(w, log, consumer.Name, denial{
			status: http.StatusBadRequest, result: metrics.ResultInvalid,
			typ: "invalid_request_error", code: "invalid_model",
			msg: fmt.Sprintf("the requested model name is longer than %d bytes", maxModel),
		})
		return
	}
	if model == "" {
		h.deny(w, log, consumer.Name, denial{
			status: http.StatusBadRequest, result: metrics.ResultInvalid,
			typ: "invalid_request_error", code: "missing_model", msg: "the requested model could not be determined",
		})
		return
	}

	w.Header().Set(HeaderConsumer, consumer.Name)
	w.Header().Set(HeaderModel, model)
	w.WriteHeader(http.StatusOK)
	h.metrics.Decisions.WithLabelValues(consumer.Name, metrics.ResultAllow).Inc()
	log.Debug("allow", "consumer", consumer.Name, "model", model)
}

// denial is a negative answer: its status, its aisa_decisions_total result and its error body.
type denial struct {
	status         int
	result         string
	typ, code, msg string
}

func denyAuth(msg string) denial {
	return denial{
		status: http.StatusUnauthorized, result: metrics.ResultDenyAuth,
		typ: "invalid_request_error", code: "invalid_api_key", msg: msg,
	}
}

func (h *Handler) deny(w http.ResponseWriter, log *slog.Logger, consumer string, d denial) {
	h.metrics.Decisions.WithLabelValues(consumer, d.result).Inc()
	log.Info("deny", "status", d.status, "consumer", consumer, "result", d.result, "reason", d.code)
	writeError(w, d.status, d.typ, d.code, d.msg)
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
// request body. The header takes precedence: the adapter sets it from the body and overwrites
// any client value (adapter rule 2). A request without a model, or with a body that is not a
// JSON object, has the model "". A model longer than maxModel is errModelTooLong, from either
// source, so an adapter's choice between the header and the body does not change the answer.
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
