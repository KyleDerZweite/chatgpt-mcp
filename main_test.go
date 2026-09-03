package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"chatgpt-mcp/internal/chatgpt"
	"chatgpt-mcp/internal/config"
)

type fakeChatGPTClient struct {
	ask      func(context.Context, string, string, int) (*chatgpt.AskResult, error)
	reply    func(context.Context, string, int) (*chatgpt.AskResult, error)
	newChat  func(context.Context) (*chatgpt.Simple, error)
	upload   func(context.Context, []string, string, int) (*chatgpt.AskResult, error)
	complete func(context.Context, string, string, time.Duration) (*chatgpt.AskResult, error)
}

func (f *fakeChatGPTClient) Ask(ctx context.Context, prompt, model string, timeoutMinutes int) (*chatgpt.AskResult, error) {
	if f.ask != nil {
		return f.ask(ctx, prompt, model, timeoutMinutes)
	}
	return &chatgpt.AskResult{Response: "asked"}, nil
}

func (f *fakeChatGPTClient) Reply(ctx context.Context, prompt string, timeoutMinutes int) (*chatgpt.AskResult, error) {
	if f.reply != nil {
		return f.reply(ctx, prompt, timeoutMinutes)
	}
	return &chatgpt.AskResult{Response: "replied"}, nil
}

func (f *fakeChatGPTClient) NewChat(ctx context.Context) (*chatgpt.Simple, error) {
	if f.newChat != nil {
		return f.newChat(ctx)
	}
	return &chatgpt.Simple{Success: true, Message: "started"}, nil
}

func (f *fakeChatGPTClient) Upload(ctx context.Context, paths []string, prompt string, timeoutMinutes int) (*chatgpt.AskResult, error) {
	if f.upload != nil {
		return f.upload(ctx, paths, prompt, timeoutMinutes)
	}
	return &chatgpt.AskResult{Response: "uploaded"}, nil
}

func (f *fakeChatGPTClient) Complete(ctx context.Context, prompt, model string, timeout time.Duration) (*chatgpt.AskResult, error) {
	if f.complete != nil {
		return f.complete(ctx, prompt, model, timeout)
	}
	return &chatgpt.AskResult{Response: "completed"}, nil
}

func connectTestServer(t *testing.T, client chatGPTClient) *mcp.ClientSession {
	t.Helper()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(client).Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	})
	return clientSession
}

func TestMCPServerListsTools(t *testing.T) {
	session := connectTestServer(t, &fakeChatGPTClient{})
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	want := map[string]bool{
		"chatgpt_ask":      false,
		"chatgpt_reply":    false,
		"chatgpt_new_chat": false,
		"chatgpt_upload":   false,
	}
	for _, tool := range result.Tools {
		if _, ok := want[tool.Name]; !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		want[tool.Name] = true
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
		if tool.OutputSchema == nil {
			t.Errorf("tool %q has no output schema", tool.Name)
		}
		if tool.Annotations == nil {
			t.Errorf("tool %q has no safety annotations", tool.Name)
		} else {
			if tool.Annotations.ReadOnlyHint || tool.Annotations.IdempotentHint {
				t.Errorf("tool %q incorrectly claims read-only/idempotent behavior", tool.Name)
			}
			if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
				t.Errorf("tool %q should declare destructiveHint=false", tool.Name)
			}
			if tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
				t.Errorf("tool %q should declare openWorldHint=true", tool.Name)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q was not listed", name)
		}
	}
}

func TestMCPStdioInitializationAndToolList(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=TestMCPStdioHelperProcess")
	command.Env = append(os.Environ(), "CHATGPT_MCP_STDIO_HELPER=1")
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("initialize stdio MCP server: %v", err)
	}
	defer session.Close()
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools over stdio: %v", err)
	}
	if len(result.Tools) != 4 {
		t.Fatalf("stdio tools/list returned %d tools, want 4", len(result.Tools))
	}
}

func TestMCPStdioHelperProcess(t *testing.T) {
	if os.Getenv("CHATGPT_MCP_STDIO_HELPER") != "1" {
		return
	}
	err := newMCPServer(&fakeChatGPTClient{}).Run(context.Background(), &mcp.StdioTransport{})
	if err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestMCPToolFailureIsToolError(t *testing.T) {
	wantErr := errors.New("browser exploded")
	session := connectTestServer(t, &fakeChatGPTClient{
		ask: func(context.Context, string, string, int) (*chatgpt.AskResult, error) {
			return nil, wantErr
		},
	})

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "chatgpt_ask",
		Arguments: map[string]any{"prompt": "hello"},
	})
	if err != nil {
		t.Fatalf("call tool returned a protocol error: %v", err)
	}
	if !result.IsError {
		t.Fatal("call tool returned isError=false")
	}
	if len(result.Content) != 1 {
		t.Fatalf("error content count = %d, want 1", len(result.Content))
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("error content type = %T, want *mcp.TextContent", result.Content[0])
	}
	if !strings.Contains(text.Text, wantErr.Error()) {
		t.Errorf("error text %q does not contain %q", text.Text, wantErr)
	}
}

func TestMCPRejectsInvalidArgumentsBeforeCallingBrowser(t *testing.T) {
	called := false
	client := &fakeChatGPTClient{
		ask: func(context.Context, string, string, int) (*chatgpt.AskResult, error) {
			called = true
			return &chatgpt.AskResult{Response: "unexpected"}, nil
		},
		upload: func(context.Context, []string, string, int) (*chatgpt.AskResult, error) {
			called = true
			return &chatgpt.AskResult{Response: "unexpected"}, nil
		},
	}
	session := connectTestServer(t, client)
	tests := []mcp.CallToolParams{
		{Name: "chatgpt_ask", Arguments: map[string]any{"prompt": "   "}},
		{Name: "chatgpt_ask", Arguments: map[string]any{"prompt": "hello", "model": "-invalid"}},
		{Name: "chatgpt_ask", Arguments: map[string]any{"prompt": "hello", "timeout_minutes": -1}},
		{Name: "chatgpt_upload", Arguments: map[string]any{"file_paths": []any{}}},
		{Name: "chatgpt_upload", Arguments: map[string]any{"file_paths": nil}},
		{Name: "chatgpt_upload", Arguments: map[string]any{"file_paths": []any{"relative.txt"}}},
	}
	for _, test := range tests {
		result, err := session.CallTool(context.Background(), &test)
		if err != nil {
			t.Fatalf("%s invalid call returned protocol error: %v", test.Name, err)
		}
		if !result.IsError {
			t.Errorf("%s accepted invalid arguments %#v", test.Name, test.Arguments)
		}
	}
	if called {
		t.Fatal("invalid MCP arguments reached the browser client")
	}
}

func TestMCPBrowserTransactionsAreSerialized(t *testing.T) {
	var (
		mu        sync.Mutex
		active    int
		maxActive int
	)
	started := make(chan string, 2)
	releaseAsk := make(chan struct{})

	begin := func(name string) func() {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		started <- name
		return func() {
			mu.Lock()
			active--
			mu.Unlock()
		}
	}

	session := connectTestServer(t, &fakeChatGPTClient{
		ask: func(ctx context.Context, _ string, _ string, _ int) (*chatgpt.AskResult, error) {
			done := begin("ask")
			defer done()
			select {
			case <-releaseAsk:
				return &chatgpt.AskResult{Response: "asked"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		newChat: func(context.Context) (*chatgpt.Simple, error) {
			done := begin("new-chat")
			defer done()
			return &chatgpt.Simple{Success: true}, nil
		},
	})

	type callResult struct {
		result *mcp.CallToolResult
		err    error
	}
	askDone := make(chan callResult, 1)
	go func() {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "chatgpt_ask",
			Arguments: map[string]any{"prompt": "first"},
		})
		askDone <- callResult{result: result, err: err}
	}()

	select {
	case name := <-started:
		if name != "ask" {
			t.Fatalf("first operation = %q, want ask", name)
		}
	case <-time.After(time.Second):
		t.Fatal("ask did not start")
	}

	newChatDone := make(chan callResult, 1)
	newChatLaunched := make(chan struct{})
	go func() {
		close(newChatLaunched)
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "chatgpt_new_chat"})
		newChatDone <- callResult{result: result, err: err}
	}()
	<-newChatLaunched

	select {
	case name := <-started:
		t.Fatalf("operation %q started while ask was still active", name)
	case <-time.After(250 * time.Millisecond):
	}

	close(releaseAsk)
	select {
	case name := <-started:
		if name != "new-chat" {
			t.Fatalf("second operation = %q, want new-chat", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("new chat did not start after ask completed")
	}

	for name, done := range map[string]<-chan callResult{"ask": askDone, "new-chat": newChatDone} {
		select {
		case call := <-done:
			if call.err != nil {
				t.Errorf("%s call: %v", name, call.err)
			} else if call.result.IsError {
				t.Errorf("%s returned a tool error", name)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s call did not finish", name)
		}
	}

	mu.Lock()
	gotMaxActive := maxActive
	mu.Unlock()
	if gotMaxActive != 1 {
		t.Errorf("maximum concurrent browser operations = %d, want 1", gotMaxActive)
	}
}

func TestQueuedBrowserOperationHonorsCancellation(t *testing.T) {
	gate := newOperationGate()
	if err := gate.acquire(context.Background()); err != nil {
		t.Fatalf("acquire gate: %v", err)
	}
	defer gate.release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	called := false
	started := time.Now()
	_, err := withBrowserOperation(ctx, gate, func(context.Context) (*chatgpt.Simple, error) {
		called = true
		return &chatgpt.Simple{Success: true}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("operation error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("canceled queued operation was invoked")
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond {
		t.Fatalf("operation returned after %s; it did not wait behind the held semaphore", elapsed)
	}
}

func TestOperationGateStopCancelsActiveOperationContext(t *testing.T) {
	gate := newOperationGate()
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := withBrowserOperation(context.Background(), gate, func(ctx context.Context) (*chatgpt.Simple, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("browser operation did not start")
	}
	drained := gate.stop()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("operation error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("service shutdown did not cancel the active operation context")
	}
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("operation tracker did not drain")
	}
}

func TestNormalizeServiceCancellation(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := normalizeServiceCancellation(fmt.Errorf("transport: %w", context.Canceled), canceled); err != nil {
		t.Fatalf("normal service cancellation = %v, want nil", err)
	}
	if err := normalizeServiceCancellation(context.Canceled, context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cancellation = %v, want context.Canceled", err)
	}
	sentinel := errors.New("transport failed")
	if err := normalizeServiceCancellation(sentinel, canceled); !errors.Is(err, sentinel) {
		t.Fatalf("non-cancellation error = %v, want sentinel", err)
	}
}

type blockingProviderCompleter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
}

func (b *blockingProviderCompleter) Complete(context.Context, string, string, time.Duration) (*chatgpt.AskResult, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	b.once.Do(func() { close(b.started) })
	<-b.release
	return &chatgpt.AskResult{Response: "done"}, nil
}

func (b *blockingProviderCompleter) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

type cancelProviderCompleter struct {
	started  chan struct{}
	finished chan struct{}
}

func (b *cancelProviderCompleter) Complete(ctx context.Context, _, _ string, _ time.Duration) (*chatgpt.AskResult, error) {
	close(b.started)
	<-ctx.Done()
	close(b.finished)
	return nil, ctx.Err()
}

func TestDrainingCompleterWaitsAndRejectsNewCalls(t *testing.T) {
	backend := &blockingProviderCompleter{started: make(chan struct{}), release: make(chan struct{})}
	completions := newDrainingCompleter(backend)
	resultCh := make(chan *chatgpt.AskResult, 1)
	go func() {
		result, _ := completions.Complete(context.Background(), "prompt", "chatgpt-auto", time.Minute)
		resultCh <- result
	}()

	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("completion did not start")
	}
	done := completions.calls.stop()
	select {
	case <-done:
		t.Fatal("drain completed while a backend call was active")
	default:
	}
	close(backend.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("drain did not complete after the backend call returned")
	}
	if result := <-resultCh; result == nil || result.Response != "done" {
		t.Fatalf("active completion result = %#v", result)
	}

	if _, err := completions.Complete(context.Background(), "late", "chatgpt-auto", time.Minute); err == nil {
		t.Fatal("completion accepted after shutdown began")
	}
	if got := backend.callCount(); got != 1 {
		t.Fatalf("backend call count = %d, want 1", got)
	}
}

func TestHTTPShutdownCancelsAndDrainsActiveCompletion(t *testing.T) {
	baseCtx, cancel := context.WithCancel(context.Background())
	backend := &cancelProviderCompleter{started: make(chan struct{}), finished: make(chan struct{})}
	completions := newDrainingCompleter(backend)
	cfg := &config.Config{
		DefaultTimeoutMinutes: 1,
		ProviderModels:        []string{"chatgpt-auto"},
		ProviderDefaultModel:  "chatgpt-auto",
	}
	server, listener, requests, err := makeHTTPServer(baseCtx, cfg, completions, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- server.Serve(listener) }()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		body := `{"model":"chatgpt-auto","messages":[{"role":"user","content":"hello"}]}`
		response, err := (&http.Client{Timeout: 2 * time.Second}).Post(
			"http://"+listener.Addr().String()+"/v1/chat/completions",
			"application/json",
			strings.NewReader(body),
		)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
	}()

	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("provider completion did not start")
	}
	if err := shutdownHTTP(server, cancel, requests, completions, nil, nil); err != nil {
		t.Fatalf("shutdownHTTP() error = %v", err)
	}
	select {
	case <-backend.finished:
	default:
		t.Fatal("shutdown returned before the active completion finished")
	}
	if err := <-serveErrCh; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve() error = %v, want http.ErrServerClosed", err)
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("HTTP request did not finish after shutdown")
	}
}

func TestHTTPShutdownIsBoundedWhenBackendIgnoresCancellation(t *testing.T) {
	baseCtx, cancel := context.WithCancel(context.Background())
	backend := &blockingProviderCompleter{started: make(chan struct{}), release: make(chan struct{})}
	defer close(backend.release)
	completions := newDrainingCompleter(backend)
	cfg := &config.Config{
		DefaultTimeoutMinutes: 1,
		ProviderModels:        []string{"chatgpt-auto"},
		ProviderDefaultModel:  "chatgpt-auto",
	}
	server, listener, requests, err := makeHTTPServer(baseCtx, cfg, completions, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	go func() {
		body := `{"model":"chatgpt-auto","messages":[{"role":"user","content":"hello"}]}`
		response, requestErr := http.Post("http://"+listener.Addr().String()+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if requestErr == nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("provider completion did not start")
	}
	aborted := make(chan struct{})
	started := time.Now()
	err = shutdownHTTPWithin(server, cancel, requests, completions, nil, func() { close(aborted) }, 50*time.Millisecond, 50*time.Millisecond)
	if err == nil {
		t.Fatal("shutdown unexpectedly reported success for a stuck backend")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded shutdown took %s", elapsed)
	}
	select {
	case <-aborted:
	default:
		t.Fatal("shutdown did not invoke the browser abort hook")
	}
}

func TestHTTPShutdownTracksAndAbortsStuckSharedBrowserOperation(t *testing.T) {
	baseCtx, cancel := context.WithCancel(context.Background())
	backend := &fakeChatGPTClient{}
	completions := newDrainingCompleter(backend)
	cfg := &config.Config{
		DefaultTimeoutMinutes: 1,
		ProviderModels:        []string{"chatgpt-auto"},
		ProviderDefaultModel:  "chatgpt-auto",
	}
	server, listener, requests, err := makeHTTPServer(baseCtx, cfg, completions, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()

	gate := newOperationGate()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	go func() {
		_, _ = withBrowserOperation(context.Background(), gate, func(context.Context) (*chatgpt.Simple, error) {
			close(started)
			<-release
			return &chatgpt.Simple{Success: true}, nil
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shared browser operation did not start")
	}
	aborted := make(chan struct{})
	err = shutdownHTTPWithin(server, cancel, requests, completions, gate.stop(), func() { close(aborted) }, 50*time.Millisecond, 50*time.Millisecond)
	if err == nil {
		t.Fatal("shutdown unexpectedly reported success for a stuck shared operation")
	}
	select {
	case <-aborted:
	default:
		t.Fatal("shutdown did not abort the browser for a stuck MCP-style operation")
	}
	if _, err := withBrowserOperation(context.Background(), gate, func(context.Context) (*chatgpt.Simple, error) {
		return &chatgpt.Simple{Success: true}, nil
	}); err == nil {
		t.Fatal("shared browser gate accepted a new operation after shutdown began")
	}
}

func TestProviderUsesSharedBrowserGate(t *testing.T) {
	gate := newOperationGate()
	if err := gate.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	backend := &fakeChatGPTClient{complete: func(context.Context, string, string, time.Duration) (*chatgpt.AskResult, error) {
		close(started)
		return &chatgpt.AskResult{Response: "done"}, nil
	}}
	completer := &gatedProviderCompleter{backend: backend, gate: gate}
	done := make(chan error, 1)
	go func() {
		_, err := completer.Complete(context.Background(), "prompt", "", time.Minute)
		done <- err
	}()
	select {
	case <-started:
		t.Fatal("provider browser operation bypassed the shared gate")
	case <-time.After(50 * time.Millisecond):
	}
	gate.release()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider browser operation did not start after the gate opened")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func writeTestTLSCertificate(t *testing.T) (string, string) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateKeyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKeyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestServeProviderUsesConfiguredTransport(t *testing.T) {
	for _, tlsEnabled := range []bool{false, true} {
		name := "HTTP"
		if tlsEnabled {
			name = "HTTPS"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, "ok")
				}),
				TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			}
			t.Cleanup(func() { _ = server.Close() })

			certFile, keyFile := "", ""
			scheme := "http"
			client := &http.Client{Timeout: 2 * time.Second}
			if tlsEnabled {
				certFile, keyFile = writeTestTLSCertificate(t)
				scheme = "https"
				client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // Test-only certificate.
			}
			serveErr := make(chan error, 1)
			go func() { serveErr <- serveProvider(server, listener, certFile, keyFile) }()
			response, err := client.Get(scheme + "://" + listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(body) != "ok" {
				t.Fatalf("response body = %q, want ok", body)
			}
			if err := server.Close(); err != nil {
				t.Fatal(err)
			}
			if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
				t.Fatalf("serveProvider() error = %v, want http.ErrServerClosed", err)
			}
		})
	}
}

func TestLoopbackListenKeepsHostRebindingProtectionWhenRemoteOptInIsSet(t *testing.T) {
	cfg := &config.Config{
		DefaultTimeoutMinutes: 1,
		ProviderModels:        []string{"chatgpt-auto"},
		ProviderDefaultModel:  "chatgpt-auto",
		ProviderAllowRemote:   true,
	}
	server, listener, _, err := makeHTTPServer(context.Background(), cfg, &fakeChatGPTClient{}, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Host = "attacker.example"
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("remote Host status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestLogProviderStartUsesConfiguredScheme(t *testing.T) {
	originalWriter := log.Writer()
	originalFlags := log.Flags()
	originalPrefix := log.Prefix()
	defer func() {
		log.SetOutput(originalWriter)
		log.SetFlags(originalFlags)
		log.SetPrefix(originalPrefix)
	}()
	log.SetFlags(0)
	log.SetPrefix("")

	for _, test := range []struct {
		name       string
		tlsEnabled bool
		want       string
	}{
		{name: "HTTP", want: "http://127.0.0.1:8787/v1"},
		{name: "HTTPS", tlsEnabled: true, want: "https://127.0.0.1:8787/v1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			log.SetOutput(&output)
			logProviderStart("127.0.0.1:8787", true, test.tlsEnabled)
			if !strings.Contains(output.String(), test.want) {
				t.Fatalf("log output = %q, want %q", output.String(), test.want)
			}
		})
	}
}

func TestValidateListen(t *testing.T) {
	strongKey := strings.Repeat("k", minimumRemoteAPIKeyBytes)
	tests := []struct {
		name        string
		addr        string
		allowRemote bool
		apiKey      string
		certFile    string
		keyFile     string
		wantErr     bool
	}{
		{name: "IPv4 loopback HTTP", addr: "127.0.0.1:8787"},
		{name: "IPv6 loopback HTTP", addr: "[::1]:8787"},
		{name: "localhost HTTP", addr: "localhost:8787"},
		{name: "trailing-dot localhost denied", addr: "localhost.:8787", wantErr: true},
		{name: "loopback permits short optional key", addr: "127.0.0.1:8787", apiKey: "short"},
		{name: "loopback TLS", addr: "127.0.0.1:8787", certFile: "server.crt", keyFile: "server.key"},
		{name: "loopback rejects certificate only", addr: "127.0.0.1:8787", certFile: "server.crt", wantErr: true},
		{name: "loopback rejects key only", addr: "127.0.0.1:8787", keyFile: "server.key", wantErr: true},
		{name: "unspecified denied", addr: "0.0.0.0:8787", apiKey: strongKey, certFile: "server.crt", keyFile: "server.key", wantErr: true},
		{name: "remote requires key", addr: "0.0.0.0:8787", allowRemote: true, certFile: "server.crt", keyFile: "server.key", wantErr: true},
		{name: "remote rejects whitespace key", addr: "0.0.0.0:8787", allowRemote: true, apiKey: "   ", certFile: "server.crt", keyFile: "server.key", wantErr: true},
		{name: "remote rejects short key", addr: "0.0.0.0:8787", allowRemote: true, apiKey: strings.Repeat("k", minimumRemoteAPIKeyBytes-1), certFile: "server.crt", keyFile: "server.key", wantErr: true},
		{name: "remote requires TLS", addr: "0.0.0.0:8787", allowRemote: true, apiKey: strongKey, wantErr: true},
		{name: "remote rejects incomplete TLS", addr: "0.0.0.0:8787", allowRemote: true, apiKey: strongKey, certFile: "server.crt", wantErr: true},
		{name: "remote explicit authenticated TLS", addr: "0.0.0.0:8787", allowRemote: true, apiKey: strongKey, certFile: "server.crt", keyFile: "server.key"},
		{name: "invalid", addr: "8787", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateListen(test.addr, test.allowRemote, test.apiKey, test.certFile, test.keyFile)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateListen() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestMCPModeHelpDoesNotStartTransport(t *testing.T) {
	t.Setenv("CHATGPT_MCP_DIR", filepath.Join(t.TempDir(), "profile"))
	t.Setenv("CHATGPT_HEADLESS", "not-a-boolean")
	if err := run([]string{"mcp", "--help"}); err != nil {
		t.Fatalf("run(mcp --help) error = %v", err)
	}
	if err := run([]string{"provider", "--help"}); err != nil {
		t.Fatalf("run(provider --help) error = %v", err)
	}
}
