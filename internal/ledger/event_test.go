package ledger

import (
	"bytes"
	"testing"
)

func TestParseEventsSingleObjectNumbers(t *testing.T) {
	input := []byte(`{
		"ts": "2026-09-28T12:00:00Z",
		"request_id": "req-123",
		"consumer": "batch-jobs",
		"model": "qwen3",
		"requested_model": "cloud-large",
		"backend": "ollama-1",
		"provider": "openai-compatible",
		"prompt_tokens": 812,
		"completion_tokens": 240,
		"status": 200,
		"latency_ms": 9120.5,
		"ttft_ms": 1450,
		"stream": true
	}`)

	events, err := ParseEvents(input)
	if err != nil {
		t.Fatalf("ParseEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.RequestID != "req-123" {
		t.Errorf("RequestID = %q, want req-123", ev.RequestID)
	}
	if ev.Consumer != "batch-jobs" {
		t.Errorf("Consumer = %q, want batch-jobs", ev.Consumer)
	}
	if ev.PromptTokens != 812 || ev.CompletionTokens != 240 {
		t.Errorf("tokens = %d/%d, want 812/240", ev.PromptTokens, ev.CompletionTokens)
	}
	if ev.Status != 200 {
		t.Errorf("Status = %d, want 200", ev.Status)
	}
	if ev.LatencyMS != 9120.5 || ev.TTFTMS != 1450 {
		t.Errorf("latency/ttft = %v/%v, want 9120.5/1450", ev.LatencyMS, ev.TTFTMS)
	}
	if !bool(ev.Stream) {
		t.Errorf("Stream = false, want true")
	}
}

func TestParseEventsStrings(t *testing.T) {
	input := []byte(`{
		"request_id": "req-str",
		"prompt_tokens": "100",
		"completion_tokens": "50",
		"status": "200",
		"latency_ms": "123.4",
		"ttft_ms": "45.6",
		"stream": "true"
	}`)

	events, err := ParseEvents(input)
	if err != nil {
		t.Fatalf("ParseEvents: %v", err)
	}
	ev := events[0]
	if ev.PromptTokens != 100 || ev.CompletionTokens != 50 {
		t.Errorf("tokens = %d/%d, want 100/50", ev.PromptTokens, ev.CompletionTokens)
	}
	if ev.Status != 200 {
		t.Errorf("Status = %d, want 200", ev.Status)
	}
	if ev.LatencyMS != 123.4 || ev.TTFTMS != 45.6 {
		t.Errorf("latency/ttft = %v/%v, want 123.4/45.6", ev.LatencyMS, ev.TTFTMS)
	}
	if !bool(ev.Stream) {
		t.Errorf("Stream = false, want true")
	}
}

func TestParseEventsNullsAndEmptyStrings(t *testing.T) {
	input := []byte(`{
		"prompt_tokens": "",
		"completion_tokens": null,
		"status": null,
		"latency_ms": "",
		"ttft_ms": null,
		"stream": ""
	}`)

	events, err := ParseEvents(input)
	if err != nil {
		t.Fatalf("ParseEvents: %v", err)
	}
	ev := events[0]
	if ev.PromptTokens != 0 || ev.CompletionTokens != 0 {
		t.Errorf("tokens = %d/%d, want 0/0", ev.PromptTokens, ev.CompletionTokens)
	}
	if ev.Status != 0 {
		t.Errorf("Status = %d, want 0", ev.Status)
	}
	if ev.LatencyMS != 0 || ev.TTFTMS != 0 {
		t.Errorf("latency/ttft = %v/%v, want 0/0", ev.LatencyMS, ev.TTFTMS)
	}
	if bool(ev.Stream) {
		t.Errorf("Stream = true, want false")
	}
}

func TestParseEventsArray(t *testing.T) {
	input := []byte(`[
		{"request_id": "r1", "status": 200, "stream": "false"},
		{"request_id": "r2", "status": "429", "stream": "1"}
	]`)

	events, err := ParseEvents(input)
	if err != nil {
		t.Fatalf("ParseEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	if events[0].RequestID != "r1" || bool(events[0].Stream) {
		t.Errorf("event 0 mismatch: %+v", events[0])
	}
	if events[1].RequestID != "r2" || events[1].Status != 429 || !bool(events[1].Stream) {
		t.Errorf("event 1 mismatch: %+v", events[1])
	}
}

func TestParseEventsIgnoresUnknownFields(t *testing.T) {
	input := []byte(`{
		"request_id": "r1",
		"authorization": "Bearer secret-leaked",
		"random_field": {"nested": true}
	}`)

	events, err := ParseEvents(input)
	if err != nil {
		t.Fatalf("ParseEvents: %v", err)
	}
	if events[0].RequestID != "r1" {
		t.Errorf("RequestID = %q, want r1", events[0].RequestID)
	}
}

func TestParseEventsRejects(t *testing.T) {
	for name, in := range map[string]string{
		"empty":               "",
		"whitespace only":     "   \n",
		"plain string":        `"hello"`,
		"number":              `123`,
		"boolean":             `true`,
		"invalid json":        `{not json`,
		"array invalid json":  `[{"r": 1}, invalid]`,
		"array trailing data": `[{"r": 1}] {}`,
		"array extra ]":       `[{"request_id": "r", "status": 200}]]`,
		"array extra }":       `[{"request_id": "r", "status": 200}]}`,
		"object extra ]":      `{"request_id": "r", "status": 200}]`,
		"object extra }":      `{"request_id": "r", "status": 200}}`,
		"object then object":  `{"request_id": "r"} {"request_id": "s"}`,
		"array unterminated":  `[{"r": 1},`,
	} {
		if _, err := ParseEvents([]byte(in)); err == nil {
			t.Errorf("%s: want error for %q", name, in)
		}
	}
}

// A well-formed event with a value that cannot be parsed is returned with its error, so that
// only that event is rejected.
func TestParseEventsValueErrors(t *testing.T) {
	for name, in := range map[string]string{
		"fractional string int": `{"request_id": "r", "prompt_tokens": "1.9"}`,
		"fractional json int":   `{"request_id": "r", "prompt_tokens": 1.9}`,
		"not a number":          `{"request_id": "r", "prompt_tokens": "abc"}`,
		"float +Inf":            `{"request_id": "r", "latency_ms": "+Inf"}`,
		"float -Inf":            `{"request_id": "r", "latency_ms": "-Inf"}`,
		"float NaN":             `{"request_id": "r", "latency_ms": "NaN"}`,
		"invalid bool":          `{"request_id": "r", "stream": "yes"}`,
		"string field a number": `{"request_id": "r", "consumer": 5}`,
	} {
		for _, body := range []string{in, "[" + in + "]"} {
			events, err := ParseEvents([]byte(body))
			if err != nil || len(events) != 1 {
				t.Errorf("%s: ParseEvents(%s) = %d events, %v; want 1 event", name, body, len(events), err)
				continue
			}
			if err := validateEvent(events[0]); err == nil {
				t.Errorf("%s: event of %s is valid, want an error", name, body)
			}
			if events[0].RequestID != "r" {
				t.Errorf("%s: request_id = %q, want r for the log", name, events[0].RequestID)
			}
		}
	}

	events, err := ParseEvents([]byte(`[1, "x", {"request_id": "ok", "status": 200}]`))
	if err != nil || len(events) != 3 {
		t.Fatalf("array with values that are not objects: %d events, %v; want 3", len(events), err)
	}
	for i, want := range []bool{false, false, true} {
		if valid := validateEvent(events[i]) == nil; valid != want {
			t.Errorf("event %d valid = %v, want %v", i, valid, want)
		}
	}
}

func TestParseEventsBatchLimit(t *testing.T) {
	// Create an array with maxBatchEvents + 1 items
	var buf bytes.Buffer
	buf.WriteString("[")
	for i := 0; i <= maxBatchEvents; i++ {
		if i > 0 {
			buf.WriteString(",")
		}
		buf.WriteString(`{"request_id":"req"}`)
	}
	buf.WriteString("]")

	if _, err := ParseEvents(buf.Bytes()); err == nil {
		t.Error("want error for batch exceeding maxBatchEvents")
	}
}
