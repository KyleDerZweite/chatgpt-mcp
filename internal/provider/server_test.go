package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"chatgpt-mcp/internal/chatgpt"
)

type fakeCompleter struct {
	mu       sync.Mutex
	prompts  []string
	models   []string
	fn       func(context.Context, string, string) *chatgpt.AskResult
	complete func(context.Context, string, string) (*chatgpt.AskResult, error)
}

type nonFlushingRecorder struct {
	recorder *httptest.ResponseRecorder
}

func (r *nonFlushingRecorder) Header() http.Header {
	return r.recorder.Header()
}

func (r *nonFlushingRecorder) Write(body []byte) (int, error) {
	return r.recorder.Write(body)
}

func (r *nonFlushingRecorder) WriteHeader(statusCode int) {
	r.recorder.WriteHeader(statusCode)
}

func (f *fakeCompleter) Complete(ctx context.Context, prompt, model string, _ time.Duration) (*chatgpt.AskResult, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	f.models = append(f.models, model)
	f.mu.Unlock()
	if f.complete != nil {
		return f.complete(ctx, prompt, model)
	}
	return f.fn(ctx, prompt, model), nil
}

func testServer(t *testing.T, backend Completer, mutate func(*Options)) *Server {
	t.Helper()
	opts := Options{
		Models:            []string{"chatgpt-auto", "gpt-5"},
		DefaultModel:      "chatgpt-auto",
		Timeout:           time.Second,
		HeartbeatInterval: time.Millisecond,
		QueueSize:         2,
	}
	if mutate != nil {
		mutate(&opts)
	}
	server, err := New(backend, opts)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func request(t *testing.T, server http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:8787"
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	return recorder
}

func TestModelsAndAuthentication(t *testing.T) {
	backend := &fakeCompleter{fn: func(context.Context, string, string) *chatgpt.AskResult {
		return &chatgpt.AskResult{Response: "unused"}
	}}
	server := testServer(t, backend, func(opts *Options) { opts.APIKey = "secret" })

	unauthorized := request(t, server, http.MethodGet, "/v1/models", "", "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
	authorized := request(t, server, http.MethodGet, "/v1/models", "", "secret")
	if authorized.Code != http.StatusOK || !strings.Contains(authorized.Body.String(), `"id":"chatgpt-auto"`) {
		t.Fatalf("unexpected models response: %d %s", authorized.Code, authorized.Body.String())
	}
}

func TestBearerAuthenticationHandlesDifferentLengthKeys(t *testing.T) {
	apiKey := strings.Repeat("k", 32)
	backend := &fakeCompleter{fn: func(context.Context, string, string) *chatgpt.AskResult {
		return &chatgpt.AskResult{Response: "unused"}
	}}
	server := testServer(t, backend, func(opts *Options) { opts.APIKey = apiKey })
	for _, test := range []struct {
		name string
		key  string
		want int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "short mismatch", key: "k", want: http.StatusUnauthorized},
		{name: "same-length mismatch", key: strings.Repeat("x", 32), want: http.StatusUnauthorized},
		{name: "long mismatch", key: strings.Repeat("k", 64), want: http.StatusUnauthorized},
		{name: "match", key: apiKey, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := request(t, server, http.MethodGet, "/v1/models", "", test.key)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestModelIDsUseHardenedSlugRules(t *testing.T) {
	backend := &fakeCompleter{fn: func(context.Context, string, string) *chatgpt.AskResult {
		return &chatgpt.AskResult{Response: "unused"}
	}}
	for _, model := range []string{".", "-", ".gpt-5", "gpt-5.", "gpt-5-", strings.Repeat("a", 65)} {
		t.Run("reject_"+model, func(t *testing.T) {
			_, err := New(backend, Options{Models: []string{model}})
			if err == nil || !strings.Contains(err.Error(), "is invalid") {
				t.Fatalf("New() accepted invalid model ID %q: %v", model, err)
			}
		})
	}
	for _, model := range []string{"a", "gpt-5", "gpt.5-pro", "A0", strings.Repeat("a", 64)} {
		t.Run("accept_"+model, func(t *testing.T) {
			if _, err := New(backend, Options{Models: []string{model}}); err != nil {
				t.Fatalf("New() rejected valid model ID %q: %v", model, err)
			}
		})
	}
}

func TestBackendStatusClassifiesReturnedDeadline(t *testing.T) {
	if got := backendStatus(context.Background(), fmt.Errorf("wrapped: %w", context.DeadlineExceeded)); got != http.StatusGatewayTimeout {
		t.Fatalf("returned deadline status = %d, want %d", got, http.StatusGatewayTimeout)
	}
	if got := backendStatus(context.Background(), errors.New("browser failed")); got != http.StatusBadGateway {
		t.Fatalf("ordinary backend error status = %d, want %d", got, http.StatusBadGateway)
	}
}

func TestCompletionMapsReturnedDeadlineToGatewayTimeout(t *testing.T) {
	backend := &fakeCompleter{complete: func(context.Context, string, string) (*chatgpt.AskResult, error) {
		return nil, fmt.Errorf("browser deadline: %w", context.DeadlineExceeded)
	}}
	server := testServer(t, backend, nil)
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`
	response := request(t, server, http.MethodPost, "/v1/chat/completions", body, "")
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusGatewayTimeout, response.Body.String())
	}
}

func TestNonStreamingCompletionRejectsResultReturnedAfterDeadline(t *testing.T) {
	backend := &fakeCompleter{complete: func(ctx context.Context, _, _ string) (*chatgpt.AskResult, error) {
		<-ctx.Done()
		return &chatgpt.AskResult{Response: "too late"}, nil
	}}
	server := testServer(t, backend, func(opts *Options) { opts.Timeout = 10 * time.Millisecond })
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`
	response := request(t, server, http.MethodPost, "/v1/chat/completions", body, "")
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusGatewayTimeout, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "too late") {
		t.Fatalf("late backend result was returned: %s", response.Body.String())
	}
}

func TestNonStreamingCompletion(t *testing.T) {
	backend := &fakeCompleter{fn: func(_ context.Context, prompt, model string) *chatgpt.AskResult {
		if !strings.Contains(prompt, `"role": "system"`) || !strings.Contains(prompt, "say hello") {
			t.Fatalf("transcript was not compiled into prompt:\n%s", prompt)
		}
		if model != "" {
			t.Fatalf("chatgpt-auto should map to an empty UI slug, got %q", model)
		}
		return &chatgpt.AskResult{Response: "hello\n\n```go\nfmt.Println()\n```"}
	}}
	server := testServer(t, backend, nil)
	body := `{"model":"chatgpt-auto","messages":[{"role":"system","content":"say hello"},{"role":"user","content":"now"}]}`
	response := request(t, server, http.MethodPost, "/v1/chat/completions", body, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var decoded struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage usage `json:"usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Object != "chat.completion" || len(decoded.Choices) != 1 || decoded.Choices[0].Message.Content == nil {
		t.Fatalf("unexpected response: %#v", decoded)
	}
	if *decoded.Choices[0].Message.Content != "hello\n\n```go\nfmt.Println()\n```" || decoded.Choices[0].FinishReason != "stop" {
		t.Fatalf("unexpected choice: %#v", decoded.Choices[0])
	}
	if response.Header().Get("X-ChatGPT-MCP-Usage") != "estimated" || decoded.Usage.PromptTokens == 0 || decoded.Usage.CompletionTokens == 0 {
		t.Fatalf("expected explicitly labelled nonzero usage estimates, header=%q usage=%#v", response.Header().Get("X-ChatGPT-MCP-Usage"), decoded.Usage)
	}
}

func TestStreamingCompletionHasKeepaliveFinishUsageAndDone(t *testing.T) {
	backend := &fakeCompleter{complete: func(ctx context.Context, _, _ string) (*chatgpt.AskResult, error) {
		select {
		case <-time.After(5 * time.Millisecond):
			return &chatgpt.AskResult{Response: "streamed"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	server := testServer(t, backend, nil)
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"stream":true,"stream_options":{"include_usage":true}}`
	response := request(t, server, http.MethodPost, "/v1/chat/completions", body, "")
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("unexpected stream headers: %d %#v", response.Code, response.Header())
	}
	if response.Header().Get("X-ChatGPT-MCP-Usage") != "estimated" {
		t.Fatalf("usage label = %q", response.Header().Get("X-ChatGPT-MCP-Usage"))
	}
	stream := response.Body.String()
	for _, want := range []string{": keep-alive", `"content":"streamed"`, `"finish_reason":"stop"`, `"choices":[]`, `"usage":`, "data: [DONE]"} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream does not contain %q:\n%s", want, stream)
		}
	}
	if strings.Contains(stream, `"prompt_tokens":0`) || strings.Contains(stream, `"completion_tokens":0`) {
		t.Fatalf("stream contains a zero usage estimate:\n%s", stream)
	}

	data := sseData(t, stream)
	if len(data) != 5 {
		t.Fatalf("SSE data event count = %d, want 5:\n%s", len(data), stream)
	}
	if data[4] != "[DONE]" {
		t.Fatalf("last SSE data event = %q, want [DONE]", data[4])
	}
	var chunks []map[string]any
	for i, payload := range data[:4] {
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("decode SSE data event %d: %v\npayload: %s", i, err, payload)
		}
		chunks = append(chunks, chunk)
	}
	firstChoice := streamChoice(t, chunks[0])
	firstDelta := streamDelta(t, firstChoice)
	if firstDelta["role"] != "assistant" || firstDelta["content"] != "" || firstChoice["finish_reason"] != nil {
		t.Fatalf("unexpected role chunk: %#v", firstChoice)
	}
	contentChoice := streamChoice(t, chunks[1])
	if content := streamDelta(t, contentChoice)["content"]; content != "streamed" || contentChoice["finish_reason"] != nil {
		t.Fatalf("unexpected content chunk: %#v", contentChoice)
	}
	finishChoice := streamChoice(t, chunks[2])
	if len(streamDelta(t, finishChoice)) != 0 || finishChoice["finish_reason"] != "stop" {
		t.Fatalf("unexpected finish chunk: %#v", finishChoice)
	}
	usageChoices, ok := chunks[3]["choices"].([]any)
	if !ok || len(usageChoices) != 0 || chunks[3]["usage"] == nil {
		t.Fatalf("unexpected usage chunk: %#v", chunks[3])
	}
}

func TestStreamingValidationErrorStaysJSON(t *testing.T) {
	called := false
	backend := &fakeCompleter{complete: func(context.Context, string, string) (*chatgpt.AskResult, error) {
		called = true
		return &chatgpt.AskResult{Response: "unused"}, nil
	}}
	server := testServer(t, backend, nil)
	body := `{"model":"gpt-5","messages":[],"stream":true}`
	response := request(t, server, http.MethodPost, "/v1/chat/completions", body, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	if called {
		t.Fatal("backend was called for a request rejected before streaming began")
	}
	if strings.Contains(response.Body.String(), "data:") {
		t.Fatalf("validation error was framed as SSE: %s", response.Body.String())
	}
}

func TestStreamingRequiresFlusherBeforeStartingSSE(t *testing.T) {
	called := false
	backend := &fakeCompleter{complete: func(context.Context, string, string) (*chatgpt.AskResult, error) {
		called = true
		return &chatgpt.AskResult{Response: "unused"}, nil
	}}
	server := testServer(t, backend, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"stream":true}`))
	req.Host = "127.0.0.1:8787"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(&nonFlushingRecorder{recorder: recorder}, req)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body: %s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	if called {
		t.Fatal("backend was called without a streaming-capable response writer")
	}
	if !strings.Contains(recorder.Body.String(), `"code":"streaming_unavailable"`) {
		t.Fatalf("missing streaming_unavailable error: %s", recorder.Body.String())
	}
}

func sseData(t *testing.T, stream string) []string {
	t.Helper()
	var data []string
	for _, line := range strings.Split(stream, "\n") {
		if strings.HasPrefix(line, "data: ") {
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
	return data
}

func streamChoice(t *testing.T, chunk map[string]any) map[string]any {
	t.Helper()
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("unexpected choices in stream chunk: %#v", chunk)
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected stream choice: %#v", choices[0])
	}
	return choice
}

func streamDelta(t *testing.T, choice map[string]any) map[string]any {
	t.Helper()
	delta, ok := choice["delta"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected stream delta: %#v", choice["delta"])
	}
	return delta
}

func TestStreamingTimeoutEmitsErrorAndDone(t *testing.T) {
	release := make(chan struct{})
	backend := &fakeCompleter{fn: func(context.Context, string, string) *chatgpt.AskResult {
		<-release
		return &chatgpt.AskResult{Response: "too late"}
	}}
	server := testServer(t, backend, func(opts *Options) {
		opts.Timeout = 10 * time.Millisecond
		opts.HeartbeatInterval = time.Millisecond
	})
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"stream":true}`
	response := request(t, server, http.MethodPost, "/v1/chat/completions", body, "")
	close(release)
	stream := response.Body.String()
	if !strings.Contains(stream, `"code":"timeout"`) || !strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("timeout stream is incomplete:\n%s", stream)
	}
}

func TestEstimateUsageIsConservativeAndNonzero(t *testing.T) {
	got := estimateUsage("hello world", "answer")
	if got.PromptTokens != len("hello world") || got.CompletionTokens != len("answer") || got.TotalTokens != got.PromptTokens+got.CompletionTokens {
		t.Fatalf("unexpected usage estimate: %#v", got)
	}
}

func TestStreamingToolCall(t *testing.T) {
	backend := &fakeCompleter{complete: func(_ context.Context, prompt, _ string) (*chatgpt.AskResult, error) {
		start := strings.Index(prompt, "CHATGPT_MCP_TOOL_CALLS_BEGIN_protocol_")
		if start < 0 {
			return nil, errors.New("tool protocol missing from prompt")
		}
		end := strings.Index(prompt[start:], ", then one JSON object")
		open := prompt[start : start+end]
		closeTag := strings.Replace(open, "_BEGIN_", "_END_", 1)
		return &chatgpt.AskResult{Response: open + `{"tool_calls":[{"name":"read_file","arguments":{"path":"README.md"}}]}` + closeTag}, nil
	}}
	server := testServer(t, backend, nil)
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"read it"}],"stream":true,"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]}`
	response := request(t, server, http.MethodPost, "/v1/chat/completions", body, "")
	stream := response.Body.String()
	for _, want := range []string{`"name":"read_file"`, `"arguments":"{\"path\":\"README.md\"}"`, `"finish_reason":"tool_calls"`, "data: [DONE]"} {
		if !strings.Contains(stream, want) {
			t.Fatalf("tool stream does not contain %q:\n%s", want, stream)
		}
	}
}

func TestQueueBackpressure(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	backend := &fakeCompleter{complete: func(ctx context.Context, _, _ string) (*chatgpt.AskResult, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return &chatgpt.AskResult{Response: "done"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	server := testServer(t, backend, func(opts *Options) {
		opts.QueueSize = 1
		opts.Timeout = time.Second
	})
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- request(t, server, http.MethodPost, "/v1/chat/completions", body, "") }()
	<-started
	second := request(t, server, http.MethodPost, "/v1/chat/completions", body, "")
	if second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") == "" {
		t.Fatalf("unexpected backpressure response: %d %s", second.Code, second.Body.String())
	}
	close(release)
	if first := <-firstDone; first.Code != http.StatusOK {
		t.Fatalf("first request failed: %d %s", first.Code, first.Body.String())
	}
}

func TestRequiredToolChoiceRejectsPlainText(t *testing.T) {
	backend := &fakeCompleter{fn: func(context.Context, string, string) *chatgpt.AskResult {
		return &chatgpt.AskResult{Response: "I did not call it"}
	}}
	server := testServer(t, backend, nil)
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"read"}],"tool_choice":"required","tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]}`
	response := request(t, server, http.MethodPost, "/v1/chat/completions", body, "")
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "requires a tool call") {
		t.Fatalf("unexpected required-tool response: %d %s", response.Code, response.Body.String())
	}
}

func TestNamedToolChoiceRejectsDifferentAllowedTool(t *testing.T) {
	backend := &fakeCompleter{fn: func(_ context.Context, prompt, _ string) *chatgpt.AskResult {
		start := strings.Index(prompt, "CHATGPT_MCP_TOOL_CALLS_BEGIN_protocol_")
		end := strings.Index(prompt[start:], ", then one JSON object")
		open := prompt[start : start+end]
		closeTag := strings.Replace(open, "_BEGIN_", "_END_", 1)
		return &chatgpt.AskResult{Response: open + `{"tool_calls":[{"name":"other_tool","arguments":{}}]}` + closeTag}
	}}
	server := testServer(t, backend, nil)
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"use the required tool"}],"tool_choice":{"type":"function","function":{"name":"required_tool"}},"tools":[{"type":"function","function":{"name":"required_tool","parameters":{"type":"object"}}},{"type":"function","function":{"name":"other_tool","parameters":{"type":"object"}}}]}`
	response := request(t, server, http.MethodPost, "/v1/chat/completions", body, "")
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), `requires \"required_tool\"`) {
		t.Fatalf("unexpected named-tool response: %d %s", response.Code, response.Body.String())
	}
}

func TestRejectsUnknownModelAndOversizedBody(t *testing.T) {
	backend := &fakeCompleter{fn: func(context.Context, string, string) *chatgpt.AskResult {
		return &chatgpt.AskResult{Response: "unused"}
	}}
	server := testServer(t, backend, func(opts *Options) { opts.MaxBodyBytes = 128 })
	unknown := request(t, server, http.MethodPost, "/v1/chat/completions", `{"model":"missing","messages":[{"role":"user","content":"hi"}]}`, "")
	if unknown.Code != http.StatusNotFound || !strings.Contains(unknown.Body.String(), "model_not_found") {
		t.Fatalf("unexpected unknown-model response: %d %s", unknown.Code, unknown.Body.String())
	}
	oversizedBody := `{"model":"gpt-5","messages":[{"role":"user","content":"` + strings.Repeat("x", 256) + `"}]}`
	oversized := request(t, server, http.MethodPost, "/v1/chat/completions", oversizedBody, "")
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, body %s", oversized.Code, oversized.Body.String())
	}
}

func TestOversizedTrailingDataReturnsPayloadTooLarge(t *testing.T) {
	backend := &fakeCompleter{fn: func(context.Context, string, string) *chatgpt.AskResult {
		return &chatgpt.AskResult{Response: "unused"}
	}}
	server := testServer(t, backend, func(opts *Options) { opts.MaxBodyBytes = 128 })
	valid := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`
	response := request(t, server, http.MethodPost, "/v1/chat/completions", valid+strings.Repeat(" ", 256), "")
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized trailing data status = %d, body %s", response.Code, response.Body.String())
	}
}

func TestRejectsUnknownStoreFieldAndTrailingJSON(t *testing.T) {
	backend := &fakeCompleter{fn: func(context.Context, string, string) *chatgpt.AskResult {
		return &chatgpt.AskResult{Response: "unused"}
	}}
	server := testServer(t, backend, nil)
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "store is unsupported",
			body: `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"store":false}`,
			want: `unknown field \"store\"`,
		},
		{
			name: "user tracking is unsupported",
			body: `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"user":"customer-123"}`,
			want: `unknown field \"user\"`,
		},
		{
			name: "second JSON value",
			body: `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]} {}`,
			want: "exactly one JSON object",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := request(t, server, http.MethodPost, "/v1/chat/completions", test.body, "")
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("unexpected response: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	backend.mu.Lock()
	callCount := len(backend.prompts)
	backend.mu.Unlock()
	if callCount != 0 {
		t.Fatalf("backend was called %d times for rejected requests", callCount)
	}
}

func TestRejectsRemoteHostAndBrowserOrigin(t *testing.T) {
	backend := &fakeCompleter{fn: func(context.Context, string, string) *chatgpt.AskResult {
		return &chatgpt.AskResult{Response: "unused"}
	}}
	server := testServer(t, backend, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "evil.test"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("remote Host status = %d", recorder.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Origin", "https://evil.test")
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("browser Origin status = %d", recorder.Code)
	}
}

func TestRejectsTrailingDotLocalhostHost(t *testing.T) {
	backend := &fakeCompleter{fn: func(context.Context, string, string) *chatgpt.AskResult {
		return &chatgpt.AskResult{Response: "unused"}
	}}
	server := testServer(t, backend, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "localhost.:8787"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("trailing-dot localhost Host status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}
