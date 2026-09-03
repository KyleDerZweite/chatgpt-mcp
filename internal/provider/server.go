package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"chatgpt-mcp/internal/chatgpt"
)

var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,62}[A-Za-z0-9])?$`)

const (
	defaultMaxBodyBytes = 2 << 20
	defaultQueueSize    = 8
)

type Completer interface {
	Complete(context.Context, string, string, time.Duration) (*chatgpt.AskResult, error)
}

type completionResult struct {
	result *chatgpt.AskResult
	err    error
}

type Options struct {
	APIKey            string
	Models            []string
	DefaultModel      string
	Timeout           time.Duration
	HeartbeatInterval time.Duration
	MaxBodyBytes      int64
	QueueSize         int
	AllowRemote       bool
	Logger            *log.Logger
}

type Server struct {
	backend      Completer
	apiKeyHash   [sha256.Size]byte
	authEnabled  bool
	models       []string
	modelSet     map[string]struct{}
	defaultModel string
	timeout      time.Duration
	heartbeat    time.Duration
	maxBodyBytes int64
	allowRemote  bool
	logger       *log.Logger
	queue        chan struct{}
	started      int64
	active       atomic.Int64
}

func New(backend Completer, opts Options) (*Server, error) {
	if backend == nil {
		return nil, fmt.Errorf("provider backend is required")
	}
	if len(opts.Models) == 0 {
		return nil, fmt.Errorf("at least one provider model is required")
	}
	modelSet := make(map[string]struct{}, len(opts.Models))
	models := make([]string, 0, len(opts.Models))
	for _, model := range opts.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, exists := modelSet[model]; exists {
			continue
		}
		if !modelIDPattern.MatchString(model) {
			return nil, fmt.Errorf("provider model %q is invalid", model)
		}
		modelSet[model] = struct{}{}
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("at least one non-empty provider model is required")
	}
	if opts.DefaultModel == "" {
		opts.DefaultModel = models[0]
	}
	if _, ok := modelSet[opts.DefaultModel]; !ok {
		return nil, fmt.Errorf("default provider model %q is not in the model registry", opts.DefaultModel)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Minute
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 15 * time.Second
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = defaultMaxBodyBytes
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultQueueSize
	}
	apiKey := strings.TrimSpace(opts.APIKey)
	server := &Server{
		backend:      backend,
		authEnabled:  apiKey != "",
		models:       models,
		modelSet:     modelSet,
		defaultModel: opts.DefaultModel,
		timeout:      opts.Timeout,
		heartbeat:    opts.HeartbeatInterval,
		maxBodyBytes: opts.MaxBodyBytes,
		allowRemote:  opts.AllowRemote,
		logger:       opts.Logger,
		queue:        make(chan struct{}, opts.QueueSize),
		started:      time.Now().Unix(),
	}
	if server.authEnabled {
		server.apiKeyHash = sha256.Sum256([]byte(apiKey))
	}
	return server, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if !s.allowRemote && !isLoopbackHost(r.Host) {
		s.writeError(w, http.StatusForbidden, "requests must use a loopback Host", "invalid_request_error", "forbidden_host")
		return
	}
	if r.Header.Get("Origin") != "" {
		s.writeError(w, http.StatusForbidden, "browser-origin requests are not accepted", "invalid_request_error", "origin_not_allowed")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/") && !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="chatgpt-mcp"`)
		s.writeError(w, http.StatusUnauthorized, "invalid or missing API key", "authentication_error", "invalid_api_key")
		return
	}

	switch {
	case r.URL.Path == "/" && r.Method == http.MethodGet:
		s.writeJSON(w, http.StatusOK, map[string]any{
			"name": "chatgpt-mcp", "mode": "openai-compatible", "api": "/v1",
		})
	case (r.URL.Path == "/healthz" || r.URL.Path == "/readyz") && r.Method == http.MethodGet:
		s.writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok", "active": s.active.Load(), "queue_capacity": cap(s.queue),
		})
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		s.handleModels(w)
	case strings.HasPrefix(r.URL.Path, "/v1/models/") && r.Method == http.MethodGet:
		s.handleModel(w, strings.TrimPrefix(r.URL.Path, "/v1/models/"))
	case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
		s.handleChatCompletions(w, r)
	case knownPath(r.URL.Path):
		w.Header().Set("Allow", allowedMethod(r.URL.Path))
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
	default:
		s.writeError(w, http.StatusNotFound, "not found", "invalid_request_error", "not_found")
	}
}

func (s *Server) handleModels(w http.ResponseWriter) {
	models := make([]map[string]any, 0, len(s.models))
	for _, model := range s.models {
		models = append(models, modelObject(model, s.started))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func (s *Server) handleModel(w http.ResponseWriter, model string) {
	if _, ok := s.modelSet[model]; !ok {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("model %q was not found", model), "invalid_request_error", "model_not_found")
		return
	}
	s.writeJSON(w, http.StatusOK, modelObject(model, s.started))
}

func modelObject(model string, created int64) map[string]any {
	return map[string]any{
		"id": model, "object": "model", "created": created, "owned_by": "chatgpt-mcp",
	}
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	defer body.Close()
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var req chatCompletionRequest
	if err := decoder.Decode(&req); err != nil {
		s.writeError(w, decodeStatus(err), "invalid JSON request: "+err.Error(), "invalid_request_error", "invalid_json")
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		s.writeError(w, decodeStatus(err), err.Error(), "invalid_request_error", "invalid_json")
		return
	}
	if req.Model == "" {
		req.Model = s.defaultModel
	}
	if _, ok := s.modelSet[req.Model]; !ok {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("model %q was not found", req.Model), "invalid_request_error", "model_not_found")
		return
	}
	if err := validateRequest(req); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_request")
		return
	}

	completionID := newID("chatcmpl")
	protocol := newToolProtocol(newID("protocol"))
	prompt, allowed, toolsEnabled, err := compilePrompt(req, protocol)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_request")
		return
	}
	select {
	case s.queue <- struct{}{}:
		defer func() { <-s.queue }()
	default:
		w.Header().Set("Retry-After", "5")
		s.writeError(w, http.StatusTooManyRequests, "provider queue is full", "rate_limit_error", "queue_full")
		return
	}
	s.active.Add(1)
	defer s.active.Add(-1)

	created := time.Now().Unix()
	requestCtx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	if req.Stream {
		s.streamCompletion(w, r.Context(), requestCtx, req, prompt, allowed, toolsEnabled, protocol, completionID, created)
		return
	}
	result, completionErr := s.backend.Complete(requestCtx, prompt, browserModel(req.Model), s.timeout)
	if completionErr == nil {
		completionErr = requestCtx.Err()
	}
	if completionErr != nil {
		s.logError(completionID, req.Model, completionErr)
		s.writeError(w, backendStatus(requestCtx, completionErr), completionErr.Error(), "api_error", "browser_error")
		return
	}
	output, calls, err := s.resultOutput(result, allowed, toolsEnabled, req, protocol)
	if err != nil {
		s.logError(completionID, req.Model, err)
		s.writeError(w, backendStatus(requestCtx, err), err.Error(), "api_error", "browser_error")
		return
	}
	if completionErr = requestCtx.Err(); completionErr != nil {
		s.logError(completionID, req.Model, completionErr)
		s.writeError(w, backendStatus(requestCtx, completionErr), completionErr.Error(), "api_error", "browser_error")
		return
	}
	s.writeCompletion(w, completionID, created, req.Model, output, calls, estimateUsage(prompt, result.Response))
}

func (s *Server) streamCompletion(w http.ResponseWriter, clientCtx, ctx context.Context, req chatCompletionRequest, prompt string, allowed map[string]toolDefinition, toolsEnabled bool, protocol toolProtocol, id string, created int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "streaming is not supported by this HTTP server", "api_error", "streaming_unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Request-ID", id)
	w.Header().Set("X-ChatGPT-MCP-Usage", "estimated")
	w.WriteHeader(http.StatusOK)
	if !writeSSE(w, map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}},
	}) {
		return
	}
	flusher.Flush()

	resultCh := make(chan completionResult, 1)
	go func() {
		result, err := s.backend.Complete(ctx, prompt, browserModel(req.Model), s.timeout)
		resultCh <- completionResult{result: result, err: err}
	}()
	ticker := time.NewTicker(s.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if clientCtx.Err() != nil {
				return
			}
			writeSSE(w, apiError{Error: apiErrorDetail{Message: "provider request timed out", Type: "api_error", Code: stringPtr("timeout")}})
			writeSSEDone(w)
			flusher.Flush()
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case completion := <-resultCh:
			if clientCtx.Err() != nil {
				return
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				writeSSE(w, apiError{Error: apiErrorDetail{Message: "provider request timed out", Type: "api_error", Code: stringPtr("timeout")}})
				writeSSEDone(w)
				flusher.Flush()
				return
			}
			if completion.err != nil {
				s.logError(id, req.Model, completion.err)
				writeSSE(w, apiError{Error: apiErrorDetail{Message: completion.err.Error(), Type: "api_error", Code: stringPtr("browser_error")}})
				writeSSEDone(w)
				flusher.Flush()
				return
			}
			result := completion.result
			output, calls, err := s.resultOutput(result, allowed, toolsEnabled, req, protocol)
			if err != nil {
				s.logError(id, req.Model, err)
				writeSSE(w, apiError{Error: apiErrorDetail{Message: err.Error(), Type: "api_error", Code: stringPtr("browser_error")}})
				writeSSEDone(w)
				flusher.Flush()
				return
			}
			if len(calls) > 0 {
				toolDeltas := make([]map[string]any, 0, len(calls))
				for i, call := range calls {
					toolDeltas = append(toolDeltas, map[string]any{
						"index": i, "id": call.ID, "type": call.Type, "function": call.Function,
					})
				}
				if !writeSSE(w, chunk(id, created, req.Model, map[string]any{"tool_calls": toolDeltas}, nil)) {
					return
				}
			} else if output != "" {
				if !writeSSE(w, chunk(id, created, req.Model, map[string]any{"content": output}, nil)) {
					return
				}
			}
			finishReason := "stop"
			if len(calls) > 0 {
				finishReason = "tool_calls"
			}
			if !writeSSE(w, chunk(id, created, req.Model, map[string]any{}, finishReason)) {
				return
			}
			if req.StreamOptions.IncludeUsage {
				estimated := estimateUsage(prompt, result.Response)
				if !writeSSE(w, map[string]any{
					"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
					"choices": []any{}, "usage": estimated,
				}) {
					return
				}
			}
			writeSSEDone(w)
			flusher.Flush()
			return
		}
	}
}

func (s *Server) resultOutput(result *chatgpt.AskResult, allowed map[string]toolDefinition, toolsEnabled bool, req chatCompletionRequest, protocol toolProtocol) (string, []responseToolCall, error) {
	if result == nil {
		return "", nil, fmt.Errorf("browser backend returned no result")
	}
	protocolResponse := result.Response
	if strings.TrimSpace(result.RawResponse) != "" {
		protocolResponse = result.RawResponse
	}
	content, calls, err := parseAssistantOutput(protocolResponse, allowed, toolsEnabled, req.ParallelToolCalls, protocol)
	if err != nil {
		return "", nil, err
	}
	if requiresToolCall(req.ToolChoice) && len(calls) == 0 {
		return "", nil, fmt.Errorf("tool_choice requires a tool call, but the model returned text")
	}
	if forced := namedToolChoice(req.ToolChoice); forced != "" {
		for _, call := range calls {
			if call.Function.Name != forced {
				return "", nil, fmt.Errorf("tool_choice requires %q, but the model requested %q", forced, call.Function.Name)
			}
		}
	}
	if len(calls) == 0 {
		format, formatErr := parseResponseFormat(req.ResponseFormat)
		if formatErr != nil {
			return "", nil, formatErr
		}
		if format == nil || format.Type == "text" {
			if result.ResponseFormatted {
				content = result.Response
			} else {
				content = protocolResponse
			}
		} else {
			content, err = normalizeResponseContent(content, req.ResponseFormat)
			if err != nil {
				return "", nil, err
			}
		}
	}
	return content, calls, nil
}

func (s *Server) writeCompletion(w http.ResponseWriter, id string, created int64, model, content string, calls []responseToolCall, estimated usage) {
	message := responseMessage{Role: "assistant"}
	finishReason := "stop"
	if len(calls) > 0 {
		message.ToolCalls = calls
		finishReason = "tool_calls"
	} else {
		message.Content = &content
	}
	w.Header().Set("X-Request-ID", id)
	w.Header().Set("X-ChatGPT-MCP-Usage", "estimated")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
		"usage":   estimated,
	})
}

func chunk(id string, created int64, model string, delta map[string]any, finishReason any) map[string]any {
	return map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
	}
}

func validateRequest(req chatCompletionRequest) error {
	if req.N != nil && *req.N != 1 {
		return fmt.Errorf("n values other than 1 are not supported")
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return fmt.Errorf("temperature must be between 0 and 2")
	}
	if req.TopP != nil && (*req.TopP < 0 || *req.TopP > 1) {
		return fmt.Errorf("top_p must be between 0 and 1")
	}
	if req.MaxTokens != nil && *req.MaxTokens <= 0 {
		return fmt.Errorf("max_tokens must be greater than zero")
	}
	if req.MaxCompletionTokens != nil && *req.MaxCompletionTokens <= 0 {
		return fmt.Errorf("max_completion_tokens must be greater than zero")
	}
	return nil
}

func requiresToolCall(choice json.RawMessage) bool {
	var value string
	if json.Unmarshal(choice, &value) == nil {
		return value == "required"
	}
	var named struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(choice, &named) == nil && named.Type == "function"
}

func browserModel(model string) string {
	if model == "chatgpt-auto" {
		return ""
	}
	return model
}

func (s *Server) authorized(r *http.Request) bool {
	if !s.authEnabled {
		return true
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	providedHash := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(providedHash[:], s.apiKeyHash[:]) == 1
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	} else if strings.Count(hostport, ":") > 1 {
		host = strings.Trim(hostport, "[]")
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (s *Server) writeError(w http.ResponseWriter, status int, message, errorType, code string) {
	s.writeJSON(w, status, apiError{Error: apiErrorDetail{
		Message: message, Type: errorType, Code: stringPtr(code),
	}})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) logError(id, model string, err error) {
	if s.logger != nil {
		// Detailed errors are returned to the authenticated/local caller. Avoid
		// copying model output or schema instance values into persistent logs.
		s.logger.Printf("request_id=%s model=%s completion_failed error_type=%T", id, model, err)
	}
}

func writeSSE(w io.Writer, value any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err == nil
}

func writeSSEDone(w io.Writer) bool {
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err == nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request body must contain exactly one JSON object")
		}
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	return nil
}

func decodeStatus(err error) int {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func backendStatus(ctx context.Context, backendErr error) int {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(backendErr, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

func knownPath(path string) bool {
	return path == "/" || path == "/healthz" || path == "/readyz" || path == "/v1/models" ||
		strings.HasPrefix(path, "/v1/models/") || path == "/v1/chat/completions"
}

func allowedMethod(path string) string {
	if path == "/v1/chat/completions" {
		return http.MethodPost
	}
	return http.MethodGet
}

func stringPtr(value string) *string {
	return &value
}

func estimateUsage(prompt, completion string) usage {
	promptTokens := approximateTokens(prompt)
	completionTokens := approximateTokens(completion)
	return usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}
}

func approximateTokens(text string) int {
	if text == "" {
		return 0
	}
	// GPT-family tokenizers have byte-level fallbacks, so one token per UTF-8
	// byte is intentionally upper-biased. Premature compaction is preferable to
	// letting a compatibility client overrun a context window we cannot measure.
	return len([]byte(text))
}

var fallbackID atomic.Uint64

func newID(prefix string) string {
	var random [12]byte
	if _, err := rand.Read(random[:]); err == nil {
		return prefix + "_" + hex.EncodeToString(random[:])
	}
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), fallbackID.Add(1))
}
