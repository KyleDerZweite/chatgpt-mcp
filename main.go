package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"chatgpt-mcp/internal/browser"
	"chatgpt-mcp/internal/chatgpt"
	"chatgpt-mcp/internal/config"
	"chatgpt-mcp/internal/provider"
)

const (
	serverName               = "chatgpt-mcp"
	serverVersion            = "0.2.0"
	gracefulShutdownTimeout  = 10 * time.Second
	forcedShutdownGrace      = 2 * time.Second
	componentStopTimeout     = 2 * time.Second
	minimumRemoteAPIKeyBytes = 32
)

type askInput struct {
	Prompt         string `json:"prompt" jsonschema:"The prompt to send to ChatGPT"`
	Model          string `json:"model,omitempty" jsonschema:"Optional ChatGPT web model slug; the current UI must expose and verify the exact selection"`
	TimeoutMinutes int    `json:"timeout_minutes,omitempty" jsonschema:"Maximum wait in minutes before giving up"`
}

type replyInput struct {
	Prompt         string `json:"prompt" jsonschema:"Follow-up prompt in the current conversation"`
	TimeoutMinutes int    `json:"timeout_minutes,omitempty" jsonschema:"Maximum wait in minutes before giving up"`
}

type uploadInput struct {
	FilePaths      []string `json:"file_paths" jsonschema:"Absolute file paths to upload"`
	Prompt         string   `json:"prompt,omitempty" jsonschema:"Optional prompt to send with the files"`
	TimeoutMinutes int      `json:"timeout_minutes,omitempty" jsonschema:"Maximum wait in minutes before giving up"`
}

// chatGPTClient is the complete set of browser transactions exposed as MCP
// tools. Keeping this boundary small lets the MCP layer be tested without
// launching a browser.
type chatGPTClient interface {
	Ask(context.Context, string, string, int) (*chatgpt.AskResult, error)
	Reply(context.Context, string, int) (*chatgpt.AskResult, error)
	NewChat(context.Context) (*chatgpt.Simple, error)
	Upload(context.Context, []string, string, int) (*chatgpt.AskResult, error)
}

type providerCompletionClient interface {
	Complete(context.Context, string, string, time.Duration) (*chatgpt.AskResult, error)
}

// operationGate serializes complete browser transactions. The channel-based
// semaphore is cancellation-aware, unlike a sync.Mutex: a request that is
// waiting behind a long-running ChatGPT operation can stop waiting as soon as
// its MCP request context is canceled.
type operationGate struct {
	semaphore     chan struct{}
	calls         *activityTracker
	serviceCtx    context.Context
	cancelService context.CancelFunc
}

func newOperationGate() *operationGate {
	serviceCtx, cancelService := context.WithCancel(context.Background())
	return &operationGate{
		semaphore:     make(chan struct{}, 1),
		calls:         newActivityTracker(),
		serviceCtx:    serviceCtx,
		cancelService: cancelService,
	}
}

func (g *operationGate) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case g.semaphore <- struct{}{}:
		// Cancellation and semaphore availability can become ready together.
		// Prefer cancellation rather than starting work the caller no longer
		// wants.
		if err := ctx.Err(); err != nil {
			g.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *operationGate) release() {
	<-g.semaphore
}

func (g *operationGate) stop() <-chan struct{} {
	done := g.calls.stop()
	g.cancelService()
	return done
}

func withBrowserOperation[T any](ctx context.Context, gate *operationGate, operation func(context.Context) (*T, error)) (*T, error) {
	if !gate.calls.begin() {
		return nil, fmt.Errorf("browser service is shutting down")
	}
	defer gate.calls.end()
	operationCtx, cancelOperation := context.WithCancel(ctx)
	stopServiceCancellation := context.AfterFunc(gate.serviceCtx, cancelOperation)
	defer func() {
		stopServiceCancellation()
		cancelOperation()
	}()
	if gate.serviceCtx.Err() != nil {
		cancelOperation()
	}
	if err := gate.acquire(operationCtx); err != nil {
		return nil, fmt.Errorf("waiting for browser operation: %w", err)
	}
	defer gate.release()

	result, err := operation(operationCtx)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("browser operation returned no result")
	}
	return result, nil
}

func boolHint(value bool) *bool {
	return &value
}

func browserMutationAnnotations(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: boolHint(false),
		IdempotentHint:  false,
		OpenWorldHint:   boolHint(true),
	}
}

func schemaInt(value int) *int { return &value }

func schemaNumber(value float64) *float64 { return &value }

func falseSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Not: &jsonschema.Schema{}}
}

func objectSchema(required []string, properties map[string]*jsonschema.Schema) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:                 "object",
		Required:             required,
		Properties:           properties,
		AdditionalProperties: falseSchema(),
	}
}

func timeoutSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "integer",
		Description: "Maximum wait in minutes; 0 uses the configured default and larger values are capped by the configured maximum",
		Minimum:     schemaNumber(0),
	}
}

func askInputSchema() *jsonschema.Schema {
	return objectSchema([]string{"prompt"}, map[string]*jsonschema.Schema{
		"prompt": {
			Type:        "string",
			Description: "The non-empty prompt to send to ChatGPT",
			MinLength:   schemaInt(1),
			Pattern:     `\S`,
		},
		"model": {
			Type:        "string",
			Description: "Optional ChatGPT web model slug; the current UI must expose and verify the exact selection",
			MinLength:   schemaInt(1),
			MaxLength:   schemaInt(64),
			Pattern:     `^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,62}[A-Za-z0-9])?$`,
		},
		"timeout_minutes": timeoutSchema(),
	})
}

func replyInputSchema() *jsonschema.Schema {
	return objectSchema([]string{"prompt"}, map[string]*jsonschema.Schema{
		"prompt": {
			Type:        "string",
			Description: "The non-empty follow-up prompt",
			MinLength:   schemaInt(1),
			Pattern:     `\S`,
		},
		"timeout_minutes": timeoutSchema(),
	})
}

func uploadInputSchema() *jsonschema.Schema {
	return objectSchema([]string{"file_paths"}, map[string]*jsonschema.Schema{
		"file_paths": {
			Type:        "array",
			Description: "Absolute local file paths; configured upload count and byte limits are enforced after canonicalization",
			MinItems:    schemaInt(1),
			Items: &jsonschema.Schema{
				Type:      "string",
				MinLength: schemaInt(1),
				Pattern:   `^(?:[A-Za-z]:[\\/]|/)`,
			},
		},
		"prompt": {
			Type:        "string",
			Description: "Optional prompt to send with the files",
		},
		"timeout_minutes": timeoutSchema(),
	})
}

func newMCPServer(client chatGPTClient) *mcp.Server {
	return newMCPServerWithGate(client, newOperationGate())
}

func newMCPServerWithGate(client chatGPTClient, gate *operationGate) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "chatgpt_ask",
		Description: "Send a prompt to ChatGPT and wait for the complete response. Optional model routing fails closed unless the current UI verifies the exact selection.",
		Annotations: browserMutationAnnotations("Ask ChatGPT"),
		InputSchema: askInputSchema(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in askInput) (*mcp.CallToolResult, *chatgpt.AskResult, error) {
		result, err := withBrowserOperation(ctx, gate, func(ctx context.Context) (*chatgpt.AskResult, error) {
			return client.Ask(ctx, in.Prompt, in.Model, in.TimeoutMinutes)
		})
		if err != nil {
			return nil, nil, fmt.Errorf("ask ChatGPT: %w", err)
		}
		return nil, result, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "chatgpt_reply",
		Description: "Send a follow-up prompt in the current ChatGPT conversation and wait for the complete response.",
		Annotations: browserMutationAnnotations("Reply in ChatGPT"),
		InputSchema: replyInputSchema(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in replyInput) (*mcp.CallToolResult, *chatgpt.AskResult, error) {
		result, err := withBrowserOperation(ctx, gate, func(ctx context.Context) (*chatgpt.AskResult, error) {
			return client.Reply(ctx, in.Prompt, in.TimeoutMinutes)
		})
		if err != nil {
			return nil, nil, fmt.Errorf("reply to ChatGPT: %w", err)
		}
		return nil, result, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "chatgpt_new_chat",
		Description: "Start a fresh ChatGPT conversation, resetting the tracked conversation state.",
		Annotations: browserMutationAnnotations("Start a new ChatGPT chat"),
		InputSchema: objectSchema(nil, map[string]*jsonschema.Schema{}),
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *chatgpt.Simple, error) {
		result, err := withBrowserOperation(ctx, gate, client.NewChat)
		if err != nil {
			return nil, nil, fmt.Errorf("start a new ChatGPT conversation: %w", err)
		}
		return nil, result, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "chatgpt_upload",
		Description: "Upload explicitly allowed local files to the current ChatGPT conversation and wait for the response. Disabled by default.",
		Annotations: browserMutationAnnotations("Upload files to ChatGPT"),
		InputSchema: uploadInputSchema(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in uploadInput) (*mcp.CallToolResult, *chatgpt.AskResult, error) {
		result, err := withBrowserOperation(ctx, gate, func(ctx context.Context) (*chatgpt.AskResult, error) {
			return client.Upload(ctx, in.FilePaths, in.Prompt, in.TimeoutMinutes)
		})
		if err != nil {
			return nil, nil, fmt.Errorf("upload files to ChatGPT: %w", err)
		}
		return nil, result, nil
	})

	return server
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		printUsage()
		return nil
	}
	if len(args) > 0 && args[0] == "--version" {
		fmt.Fprintln(os.Stdout, serverVersion)
		return nil
	}

	mode := "mcp"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		mode = args[0]
		args = args[1:]
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		switch mode {
		case "mcp", "provider", "serve", "both":
			printUsage()
			return nil
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	session := browser.New(cfg)
	defer func() {
		if err := session.Close(); err != nil {
			log.Printf("close browser: %v", err)
		}
	}()
	client := chatgpt.New(cfg, session)
	gate := newOperationGate()

	switch mode {
	case "mcp":
		if len(args) != 0 {
			return fmt.Errorf("mcp mode does not accept arguments")
		}
		return runMCPService(ctx, client, gate, session.Abort)
	case "provider", "serve", "both":
		flags := flag.NewFlagSet(mode, flag.ContinueOnError)
		listen := flags.String("listen", cfg.ProviderAddr, "provider listen address")
		if err := flags.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
		}
		if err := validateListen(*listen, cfg.ProviderAllowRemote, cfg.ProviderAPIKey, cfg.ProviderTLSCertFile, cfg.ProviderTLSKeyFile); err != nil {
			return err
		}
		completer := &gatedProviderCompleter{backend: client, gate: gate}
		if mode == "both" {
			return runBoth(ctx, cfg, client, gate, completer, session.Abort, *listen)
		}
		return runProvider(ctx, cfg, gate, completer, session.Abort, *listen)
	default:
		printUsage()
		return fmt.Errorf("unknown mode %q", mode)
	}
}

type gatedProviderCompleter struct {
	backend providerCompletionClient
	gate    *operationGate
}

func (c *gatedProviderCompleter) Complete(ctx context.Context, prompt, model string, timeout time.Duration) (*chatgpt.AskResult, error) {
	return withBrowserOperation(ctx, c.gate, func(ctx context.Context) (*chatgpt.AskResult, error) {
		return c.backend.Complete(ctx, prompt, model, timeout)
	})
}

func runMCP(ctx context.Context, client chatGPTClient, gate *operationGate) error {
	return newMCPServerWithGate(client, gate).Run(ctx, &mcp.StdioTransport{})
}

func runMCPService(ctx context.Context, client chatGPTClient, gate *operationGate, abort func()) error {
	errCh := make(chan error, 1)
	go func() { errCh <- runMCP(ctx, client, gate) }()
	var runErr error
	runDone := false
	select {
	case runErr = <-errCh:
		runDone = true
	case <-ctx.Done():
	}
	runErr = normalizeServiceCancellation(runErr, ctx)
	drainErr := drainBrowserOperations(gate.stop(), abort, gracefulShutdownTimeout, forcedShutdownGrace)
	if !runDone {
		componentErr := waitComponent(errCh, "MCP transport")
		if !(errors.Is(componentErr, context.Canceled) && ctx.Err() != nil) {
			runErr = errors.Join(runErr, componentErr)
		}
	}
	return errors.Join(runErr, drainErr)
}

func normalizeServiceCancellation(err error, serviceCtx context.Context) error {
	if errors.Is(err, context.Canceled) && serviceCtx.Err() != nil {
		return nil
	}
	return err
}

func runProvider(ctx context.Context, cfg *config.Config, gate *operationGate, backend provider.Completer, abort func(), addr string) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	completions := newDrainingCompleter(backend)
	httpServer, listener, requests, err := makeHTTPServer(runCtx, cfg, completions, addr)
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- serveProvider(httpServer, listener, cfg.ProviderTLSCertFile, cfg.ProviderTLSKeyFile)
	}()
	logProviderStart(listener.Addr().String(), cfg.ProviderAPIKey != "", providerTLSEnabled(cfg.ProviderTLSCertFile, cfg.ProviderTLSKeyFile))

	serveDone := false
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
		serveDone = true
		runErr = normalizeHTTPServeError(runErr)
	}
	shutdownErr := shutdownHTTP(httpServer, cancelRun, requests, completions, gate.stop(), abort)
	if !serveDone {
		runErr = errors.Join(runErr, normalizeHTTPServeError(waitComponent(errCh, "provider listener")))
	}
	return errors.Join(runErr, shutdownErr)
}

func runBoth(ctx context.Context, cfg *config.Config, mcpClient chatGPTClient, gate *operationGate, backend provider.Completer, abort func(), addr string) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	completions := newDrainingCompleter(backend)
	httpServer, listener, requests, err := makeHTTPServer(runCtx, cfg, completions, addr)
	if err != nil {
		return err
	}
	httpErrCh := make(chan error, 1)
	mcpErrCh := make(chan error, 1)
	go func() {
		httpErrCh <- serveProvider(httpServer, listener, cfg.ProviderTLSCertFile, cfg.ProviderTLSKeyFile)
	}()
	go func() { mcpErrCh <- runMCP(runCtx, mcpClient, gate) }()
	logProviderStart(listener.Addr().String(), cfg.ProviderAPIKey != "", providerTLSEnabled(cfg.ProviderTLSCertFile, cfg.ProviderTLSKeyFile))
	log.Printf("MCP stdio transport enabled alongside the provider")

	httpDone := false
	mcpDone := false
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-httpErrCh:
		httpDone = true
		runErr = normalizeHTTPServeError(runErr)
	case runErr = <-mcpErrCh:
		mcpDone = true
		if errors.Is(runErr, context.Canceled) && runCtx.Err() != nil {
			runErr = nil
		}
	}
	shutdownErr := shutdownHTTP(httpServer, cancelRun, requests, completions, gate.stop(), abort)
	if !httpDone {
		runErr = errors.Join(runErr, normalizeHTTPServeError(waitComponent(httpErrCh, "provider listener")))
	}
	if !mcpDone {
		if err := waitComponent(mcpErrCh, "MCP transport"); err != nil && !errors.Is(err, context.Canceled) {
			runErr = errors.Join(runErr, err)
		}
	}
	return errors.Join(runErr, shutdownErr)
}

type activityTracker struct {
	mu       sync.Mutex
	active   int
	stopping bool
	done     chan struct{}
}

func newActivityTracker() *activityTracker {
	return &activityTracker{done: make(chan struct{})}
}

func (t *activityTracker) begin() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopping {
		return false
	}
	t.active++
	return true
}

func (t *activityTracker) end() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active--
	if t.stopping && t.active == 0 {
		close(t.done)
	}
}

func (t *activityTracker) stop() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.stopping {
		t.stopping = true
		if t.active == 0 {
			close(t.done)
		}
	}
	return t.done
}

type drainingCompleter struct {
	backend provider.Completer
	calls   *activityTracker
}

func newDrainingCompleter(backend provider.Completer) *drainingCompleter {
	return &drainingCompleter{backend: backend, calls: newActivityTracker()}
}

func (d *drainingCompleter) Complete(ctx context.Context, prompt, model string, timeout time.Duration) (*chatgpt.AskResult, error) {
	if !d.calls.begin() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("provider is shutting down")
	}
	defer d.calls.end()
	return d.backend.Complete(ctx, prompt, model, timeout)
}

func makeHTTPServer(baseCtx context.Context, cfg *config.Config, backend provider.Completer, addr string) (*http.Server, net.Listener, *activityTracker, error) {
	if err := validateListen(addr, cfg.ProviderAllowRemote, cfg.ProviderAPIKey, cfg.ProviderTLSCertFile, cfg.ProviderTLSKeyFile); err != nil {
		return nil, nil, nil, err
	}
	host, _, _ := net.SplitHostPort(addr) // validateListen already checked this value.
	handler, err := provider.New(backend, provider.Options{
		APIKey:            cfg.ProviderAPIKey,
		Models:            cfg.ProviderModels,
		DefaultModel:      cfg.ProviderDefaultModel,
		Timeout:           time.Duration(cfg.DefaultTimeoutMinutes) * time.Minute,
		HeartbeatInterval: 15 * time.Second,
		AllowRemote:       !isLoopbackAddress(host),
		Logger:            log.Default(),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	requests := newActivityTracker()
	trackedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requests.begin() {
			http.Error(w, "provider is shutting down", http.StatusServiceUnavailable)
			return
		}
		defer requests.end()
		handler.ServeHTTP(w, r)
	})
	server := &http.Server{
		Handler:           trackedHandler,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	return server, listener, requests, nil
}

func providerTLSEnabled(certFile, keyFile string) bool {
	return strings.TrimSpace(certFile) != "" && strings.TrimSpace(keyFile) != ""
}

func serveProvider(server *http.Server, listener net.Listener, certFile, keyFile string) error {
	defer listener.Close()
	certFile = strings.TrimSpace(certFile)
	keyFile = strings.TrimSpace(keyFile)
	if (certFile == "") != (keyFile == "") {
		return fmt.Errorf("CHATGPT_PROVIDER_TLS_CERT_FILE and CHATGPT_PROVIDER_TLS_KEY_FILE must be set together")
	}
	if certFile != "" {
		return server.ServeTLS(listener, certFile, keyFile)
	}
	return server.Serve(listener)
}

func shutdownHTTP(server *http.Server, cancel context.CancelFunc, requests *activityTracker, completions *drainingCompleter, operationDone <-chan struct{}, abort func()) error {
	return shutdownHTTPWithin(server, cancel, requests, completions, operationDone, abort, gracefulShutdownTimeout, forcedShutdownGrace)
}

func shutdownHTTPWithin(server *http.Server, cancel context.CancelFunc, requests *activityTracker, completions *drainingCompleter, operationDone <-chan struct{}, abort func(), gracefulTimeout, abortGrace time.Duration) error {
	cancel()
	requestDone := requests.stop()
	completionDone := completions.calls.stop()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), gracefulTimeout)
	shutdownErr := server.Shutdown(shutdownCtx)
	drainErr := waitForActivity(shutdownCtx, requestDone, completionDone, operationDone)
	cancelShutdown()
	if drainErr == nil {
		return shutdownErr
	}

	if abort != nil {
		abort()
	}
	if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
		shutdownErr = errors.Join(shutdownErr, closeErr)
	}
	abortCtx, cancelAbort := context.WithTimeout(context.Background(), abortGrace)
	abortErr := waitForActivity(abortCtx, requestDone, completionDone, operationDone)
	cancelAbort()
	if abortErr != nil {
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("provider activity did not stop after browser abort: %w", abortErr))
	} else if shutdownErr == nil {
		shutdownErr = fmt.Errorf("graceful provider shutdown exceeded %s", gracefulTimeout)
	}
	return shutdownErr
}

func drainBrowserOperations(operationDone <-chan struct{}, abort func(), gracefulTimeout, abortGrace time.Duration) error {
	gracefulCtx, cancelGraceful := context.WithTimeout(context.Background(), gracefulTimeout)
	err := waitForActivity(gracefulCtx, nil, nil, operationDone)
	cancelGraceful()
	if err == nil {
		return nil
	}
	if abort != nil {
		abort()
	}
	abortCtx, cancelAbort := context.WithTimeout(context.Background(), abortGrace)
	abortErr := waitForActivity(abortCtx, nil, nil, operationDone)
	cancelAbort()
	if abortErr != nil {
		return fmt.Errorf("browser operations did not stop after terminal abort: %w", abortErr)
	}
	return fmt.Errorf("graceful browser-operation shutdown exceeded %s", gracefulTimeout)
}

func waitForActivity(ctx context.Context, requestDone, completionDone, operationDone <-chan struct{}) error {
	for requestDone != nil || completionDone != nil || operationDone != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-requestDone:
			requestDone = nil
		case <-completionDone:
			completionDone = nil
		case <-operationDone:
			operationDone = nil
		}
	}
	return nil
}

func waitComponent(ch <-chan error, component string) error {
	timer := time.NewTimer(componentStopTimeout)
	defer timer.Stop()
	select {
	case err := <-ch:
		return err
	case <-timer.C:
		return fmt.Errorf("%s did not stop within %s", component, componentStopTimeout)
	}
}

func normalizeHTTPServeError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func validateListen(addr string, allowRemote bool, apiKey, tlsCertFile, tlsKeyFile string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid provider listen address %q: %w", addr, err)
	}
	certConfigured := strings.TrimSpace(tlsCertFile) != ""
	keyConfigured := strings.TrimSpace(tlsKeyFile) != ""
	if certConfigured != keyConfigured {
		return fmt.Errorf("CHATGPT_PROVIDER_TLS_CERT_FILE and CHATGPT_PROVIDER_TLS_KEY_FILE must be set together")
	}
	if isLoopbackAddress(host) {
		return nil
	}
	if !allowRemote {
		return fmt.Errorf("refusing non-loopback provider address %q; set CHATGPT_PROVIDER_ALLOW_REMOTE=true to opt in", addr)
	}
	trimmedAPIKey := strings.TrimSpace(apiKey)
	if trimmedAPIKey == "" {
		return fmt.Errorf("CHATGPT_PROVIDER_API_KEY is required for a non-loopback provider address")
	}
	if len([]byte(trimmedAPIKey)) < minimumRemoteAPIKeyBytes {
		return fmt.Errorf("CHATGPT_PROVIDER_API_KEY must be at least %d bytes for a non-loopback provider address", minimumRemoteAPIKeyBytes)
	}
	if !certConfigured {
		return fmt.Errorf("CHATGPT_PROVIDER_TLS_CERT_FILE and CHATGPT_PROVIDER_TLS_KEY_FILE are required for a non-loopback provider address")
	}
	return nil
}

func isLoopbackAddress(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func logProviderStart(addr string, authenticated, tlsEnabled bool) {
	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	log.Printf("OpenAI-compatible provider listening at %s://%s/v1", scheme, addr)
	if !authenticated {
		log.Printf("provider authentication is disabled; the listener is local-only")
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage:
  chatgpt-mcp                 Run the MCP stdio server (backward-compatible default)
  chatgpt-mcp mcp             Run the MCP stdio server
  chatgpt-mcp provider        Run the OpenAI-compatible HTTP/HTTPS provider
  chatgpt-mcp serve           Alias for provider mode
  chatgpt-mcp both            Run both transports in one process

Provider options:
  --listen ADDRESS            Override CHATGPT_PROVIDER_ADDR (default 127.0.0.1:8787)`)
}
