package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Event is the normalized schema of a finished request that a gateway reports (contract 2).
type Event struct {
	Timestamp        string      `json:"ts"`
	RequestID        string      `json:"request_id"`
	Consumer         string      `json:"consumer"`
	Model            string      `json:"model"`
	RequestedModel   string      `json:"requested_model"`
	Backend          string      `json:"backend"`
	Provider         string      `json:"provider"`
	PromptTokens     FlexInt64   `json:"prompt_tokens"`
	CompletionTokens FlexInt64   `json:"completion_tokens"`
	Status           FlexInt     `json:"status"`
	LatencyMS        FlexFloat64 `json:"latency_ms"`
	TTFTMS           FlexFloat64 `json:"ttft_ms"`
	Stream           FlexBool    `json:"stream"`
}

// FlexInt64 unmarshals an int64 from a JSON number, a string (e.g. "812"), null or "".
type FlexInt64 int64

// UnmarshalJSON implements json.Unmarshaler.
func (f *FlexInt64) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*f = 0
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*f = 0
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			fl, ferr := strconv.ParseFloat(s, 64)
			if ferr != nil {
				return fmt.Errorf("invalid int: %q", s)
			}
			*f = FlexInt64(fl)
			return nil
		}
		*f = FlexInt64(n)
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		var fl float64
		if json.Unmarshal(data, &fl) == nil {
			*f = FlexInt64(fl)
			return nil
		}
		return err
	}
	*f = FlexInt64(n)
	return nil
}

// FlexInt unmarshals an int from a JSON number, a string, null or "".
type FlexInt int

// UnmarshalJSON implements json.Unmarshaler.
func (f *FlexInt) UnmarshalJSON(data []byte) error {
	var v FlexInt64
	if err := v.UnmarshalJSON(data); err != nil {
		return err
	}
	*f = FlexInt(v)
	return nil
}

// FlexFloat64 unmarshals a float64 from a JSON number, a string (e.g. "9120.5"), null or "".
type FlexFloat64 float64

// UnmarshalJSON implements json.Unmarshaler.
func (f *FlexFloat64) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*f = 0
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*f = 0
			return nil
		}
		fl, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("invalid float: %q", s)
		}
		*f = FlexFloat64(fl)
		return nil
	}
	var fl float64
	if err := json.Unmarshal(data, &fl); err != nil {
		return err
	}
	*f = FlexFloat64(fl)
	return nil
}

// FlexBool unmarshals a boolean from a JSON boolean, a string ("true", "false", "1", "0"), null or "".
type FlexBool bool

// UnmarshalJSON implements json.Unmarshaler.
func (f *FlexBool) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*f = false
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(strings.ToLower(s))
		switch s {
		case "", "false", "0":
			*f = false
		case "true", "1":
			*f = true
		default:
			return fmt.Errorf("invalid bool: %q", s)
		}
		return nil
	}
	var b bool
	if err := json.Unmarshal(data, &b); err != nil {
		return err
	}
	*f = FlexBool(b)
	return nil
}

// ParseEvents parses body as either a single JSON object or an array of JSON objects.
func ParseEvents(body []byte) ([]Event, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("empty request body")
	}
	if trimmed[0] == '[' {
		var rawEvents []json.RawMessage
		if err := json.Unmarshal(trimmed, &rawEvents); err != nil {
			return nil, fmt.Errorf("invalid JSON array: %w", err)
		}
		events := make([]Event, len(rawEvents))
		for i, raw := range rawEvents {
			rawTrimmed := bytes.TrimSpace(raw)
			if len(rawTrimmed) == 0 || rawTrimmed[0] != '{' {
				return nil, fmt.Errorf("event %d is not a JSON object", i)
			}
			if err := json.Unmarshal(rawTrimmed, &events[i]); err != nil {
				return nil, fmt.Errorf("event %d: %w", i, err)
			}
		}
		return events, nil
	}
	if trimmed[0] != '{' {
		return nil, errors.New("body is not a JSON object or an array of objects")
	}
	var ev Event
	if err := json.Unmarshal(trimmed, &ev); err != nil {
		return nil, fmt.Errorf("invalid JSON object: %w", err)
	}
	return []Event{ev}, nil
}
