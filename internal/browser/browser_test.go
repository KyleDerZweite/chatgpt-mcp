package browser

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"

	"chatgpt-mcp/internal/config"
)

type reportedRemoteConn struct {
	net.Conn
	remote net.Addr
}

func (c *reportedRemoteConn) RemoteAddr() net.Addr {
	return c.remote
}

func TestValidateChatGPTURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rawURL  string
		allowed bool
	}{
		{name: "bare origin", rawURL: "https://chatgpt.com", allowed: true},
		{name: "path", rawURL: "https://chatgpt.com/c/123?model=gpt-5#response", allowed: true},
		{name: "case insensitive host", rawURL: "HTTPS://CHATGPT.COM/", allowed: true},
		{name: "explicit default port", rawURL: "https://chatgpt.com:443/", allowed: false},
		{name: "empty explicit port", rawURL: "https://chatgpt.com:/", allowed: false},
		{name: "http", rawURL: "http://chatgpt.com/", allowed: false},
		{name: "non-default port", rawURL: "https://chatgpt.com:444/", allowed: false},
		{name: "subdomain", rawURL: "https://www.chatgpt.com/", allowed: false},
		{name: "prefixed subdomain", rawURL: "https://evil.chatgpt.com/", allowed: false},
		{name: "suffix attack", rawURL: "https://chatgpt.com.evil.example/", allowed: false},
		{name: "userinfo confusion", rawURL: "https://chatgpt.com@evil.example/", allowed: false},
		{name: "userinfo on expected host", rawURL: "https://user@chatgpt.com/", allowed: false},
		{name: "trailing dot", rawURL: "https://chatgpt.com./", allowed: false},
		{name: "lookalike", rawURL: "https://chatgptXcom/", allowed: false},
		{name: "javascript", rawURL: "javascript:https://chatgpt.com", allowed: false},
		{name: "relative URL", rawURL: "/chatgpt.com", allowed: false},
		{name: "empty", rawURL: "", allowed: false},
		{name: "malformed port", rawURL: "https://chatgpt.com:bad/", allowed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateChatGPTURL(tt.rawURL)
			if tt.allowed && err != nil {
				t.Fatalf("ValidateChatGPTURL(%q) returned error: %v", tt.rawURL, err)
			}
			if !tt.allowed && err == nil {
				t.Fatalf("ValidateChatGPTURL(%q) unexpectedly succeeded", tt.rawURL)
			}
			if !tt.allowed && !errors.Is(err, ErrUntrustedChatGPTOrigin) {
				t.Fatalf("ValidateChatGPTURL(%q) error = %v, want ErrUntrustedChatGPTOrigin", tt.rawURL, err)
			}
		})
	}
}

func TestEnsureContextHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := (&Session{}).EnsureContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsureContext() error = %v, want context.Canceled", err)
	}
}

func TestEnsureCanceledContextPreservesExistingSession(t *testing.T) {
	r := rod.New()
	page := new(rod.Page)
	session := &Session{cfg: &config.Config{}, rod: r, owned: true, Page: page}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := session.EnsureContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsureContext() error = %v, want context cancellation", err)
	}
	if session.rod != r || session.Page != page || !session.owned {
		t.Fatal("canceled EnsureContext mutated the existing browser session")
	}
}

func TestNavigateContextRejectsUntrustedDestinationBeforeNavigation(t *testing.T) {
	t.Parallel()

	err := (&Session{}).NavigateContext(context.Background(), "https://evil.chatgpt.com/")
	if !errors.Is(err, ErrUntrustedChatGPTOrigin) {
		t.Fatalf("NavigateContext() error = %v, want ErrUntrustedChatGPTOrigin", err)
	}
}

func TestAssertChatGPTOriginHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := (&Session{}).AssertChatGPTOrigin(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AssertChatGPTOrigin() error = %v, want context.Canceled", err)
	}
}

func TestValidateChatGPTURLErrorDoesNotEchoCredentials(t *testing.T) {
	t.Parallel()

	rawURL := "https://sensitive-password@chatgpt.com/"
	err := ValidateChatGPTURL(rawURL)
	if err == nil {
		t.Fatal("expected URL containing userinfo to be rejected")
	}
	if strings.Contains(err.Error(), "sensitive-password") {
		t.Fatalf("error exposes URL userinfo: %v", err)
	}
}

func TestResolveControlURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			t.Fatalf("discovery path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/id"}`))
	}))
	defer server.Close()

	got, err := resolveControlURL(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := "ws" + server.URL[len("http"):] + "/devtools/browser/id"
	if got != want {
		t.Fatalf("resolved URL = %q, want %q", got, want)
	}
	direct := "wss://127.0.0.1:9222/devtools/browser/id?token=x"
	if got, err := resolveControlURL(context.Background(), direct); err != nil || got != direct {
		t.Fatalf("direct WebSocket URL = %q, %v", got, err)
	}
}

func TestConnectWebSocketHandshakeHonorsCancellation(t *testing.T) {
	for _, scheme := range []string{"ws", "wss"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			accepted := make(chan net.Conn, 1)
			go func() {
				connection, acceptErr := listener.Accept()
				if acceptErr == nil {
					accepted <- connection
				}
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, browserCancel, transport, err := connect(ctx, scheme+"://"+listener.Addr().String()+"/devtools/browser/stalled")
			if browserCancel != nil || transport != nil {
				t.Fatal("stalled connection returned live browser handles")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("connect() error = %v, want context deadline exceeded", err)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("stalled WebSocket handshake returned after %s", elapsed)
			}
			select {
			case connection := <-accepted:
				_ = connection.Close()
			case <-time.After(time.Second):
				t.Fatal("test server did not accept the WebSocket connection")
			}
		})
	}
}

func TestLoopbackWebSocketValidatesDialedPeer(t *testing.T) {
	endpoint, err := url.Parse("ws://localhost:9222/devtools/browser/id")
	if err != nil {
		t.Fatal(err)
	}
	dialer, _, err := newCDPWebSocketDialer(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer server.Close()
	dialer.dialContext = func(context.Context, string, string) (net.Conn, error) {
		return &reportedRemoteConn{
			Conn:   client,
			remote: &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 9222},
		}, nil
	}
	if _, err := dialer.DialContext(context.Background(), "tcp", endpoint.Host); err == nil || !strings.Contains(err.Error(), "outside loopback") {
		t.Fatalf("DialContext() error = %v, want non-loopback peer rejection", err)
	}
}

func TestSecureWebSocketDialerUsesVerifiedTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	endpoint, err := url.Parse("wss" + strings.TrimPrefix(server.URL, "https"))
	if err != nil {
		t.Fatal(err)
	}
	dialer, normalizedURL, err := newCDPWebSocketDialer(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if dialer.tlsConfig.InsecureSkipVerify {
		t.Fatal("WSS dialer disabled certificate verification")
	}
	if dialer.tlsConfig.MinVersion < tls.VersionTLS12 {
		t.Fatalf("WSS minimum TLS version = %x, want TLS 1.2 or newer", dialer.tlsConfig.MinVersion)
	}
	if dialer.tlsConfig.ServerName != endpoint.Hostname() {
		t.Fatalf("WSS server name = %q, want %q", dialer.tlsConfig.ServerName, endpoint.Hostname())
	}
	certificatePool := x509.NewCertPool()
	certificatePool.AddCert(server.Certificate())
	dialer.tlsConfig.RootCAs = certificatePool
	parsedNormalized, err := url.Parse(normalizedURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := dialer.DialContext(ctx, "tcp", parsedNormalized.Host)
	if err != nil {
		t.Fatalf("verified WSS dial: %v", err)
	}
	dialer.stopSetup()
	_ = connection.Close()
}

func TestCDPEndpointLoopbackClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
		remote   bool
	}{
		{name: "localhost", endpoint: "http://localhost:9222", remote: false},
		{name: "uppercase localhost", endpoint: "http://LOCALHOST:9222", remote: false},
		{name: "IPv4 loopback", endpoint: "http://127.0.0.1:9222", remote: false},
		{name: "IPv6 loopback", endpoint: "http://[::1]:9222", remote: false},
		{name: "trailing-dot localhost", endpoint: "http://localhost.:9222", remote: true},
		{name: "remote host", endpoint: "https://browser.example", remote: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsRemoteCDPEndpoint(tt.endpoint); got != tt.remote {
				t.Fatalf("IsRemoteCDPEndpoint(%q) = %t, want %t", tt.endpoint, got, tt.remote)
			}
		})
	}

	if _, err := resolveControlURL(context.Background(), "http://localhost.:9222"); err == nil || !strings.Contains(err.Error(), "refusing non-loopback") {
		t.Fatalf("trailing-dot localhost error = %v, want non-loopback rejection", err)
	}
}

func TestLoopbackCDPDiscoveryBypassesProxy(t *testing.T) {
	t.Parallel()

	var targetHits atomic.Int64
	var proxyHits atomic.Int64
	var receivedSecrets atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		user, password, hasAuth := r.BasicAuth()
		receivedSecrets.Store(hasAuth && user == "sensitive-user" && password == "sensitive-password" && r.URL.Query().Get("token") == "sensitive-query")
		_, _ = fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://%s/devtools/browser/id"}`, r.Host)
	}))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		http.Error(w, "proxy must not receive loopback discovery", http.StatusBadGateway)
	}))
	defer proxy.Close()

	endpoint, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.User = url.UserPassword("sensitive-user", "sensitive-password")
	endpoint.RawQuery = "token=sensitive-query"
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	if _, err := resolveControlURLWithPolicy(context.Background(), endpoint.String(), false, client); err != nil {
		t.Fatalf("loopback discovery: %v", err)
	}
	if got := proxyHits.Load(); got != 0 {
		t.Fatalf("proxy received %d loopback discovery requests, want 0", got)
	}
	if got := targetHits.Load(); got != 1 {
		t.Fatalf("target received %d discovery requests, want 1", got)
	}
	if !receivedSecrets.Load() {
		t.Fatal("target did not receive the expected direct authenticated discovery request")
	}
}

func TestLoopbackCDPDiscoveryValidatesDialedAddress(t *testing.T) {
	t.Parallel()

	var handlerHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerHits.Add(1)
		_, _ = fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://%s/devtools/browser/id"}`, r.Host)
	}))
	defer target.Close()

	dialer := new(net.Dialer)
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			connection, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &reportedRemoteConn{
				Conn:   connection,
				remote: &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 9222},
			}, nil
		},
	}}
	_, err := resolveControlURLWithPolicy(context.Background(), target.URL, false, client)
	if err == nil || !strings.Contains(err.Error(), "resolved outside loopback") {
		t.Fatalf("non-loopback dial error = %v, want resolved-outside-loopback rejection", err)
	}
	if got := handlerHits.Load(); got != 0 {
		t.Fatalf("target handler received %d requests before address validation, want 0", got)
	}
}

func TestResolveConfiguredControlURLAuto(t *testing.T) {
	browserPath := "/devtools/browser/current-profile"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.Error(w, "unexpected discovery path", http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://%s%s"}`, r.Host, browserPath)
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(serverURL.Port())
	if err != nil {
		t.Fatal(err)
	}
	profileDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileDir, devToolsActivePortFilename), []byte(fmt.Sprintf("%d\r\n%s\r\n", port, browserPath)), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveConfiguredControlURL(context.Background(), " auto ", profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "ws" || parsed.Hostname() != serverURL.Hostname() || parsed.Port() != strconv.Itoa(port) || parsed.Path != browserPath {
		t.Fatalf("resolved auto URL = %q, want loopback port %d and path %q", got, port, browserPath)
	}
}

func TestAutoDiscoveryRejectsMismatchedBrowser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://%s/devtools/browser/different"}`, r.Host)
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(serverURL.Port())
	if err != nil {
		t.Fatal(err)
	}
	_, err = probeAutoControlEndpoints(context.Background(), activePortInfo{port: uint16(port), browserPath: "/devtools/browser/expected"})
	if err == nil || !strings.Contains(err.Error(), "different browser target") {
		t.Fatalf("auto discovery error = %v, want target mismatch", err)
	}
}

func TestAutoDiscoverySupportsIPv6Loopback(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	browserPath := "/devtools/browser/ipv6"
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://[::1]:%d%s"}`, port, browserPath)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	profileDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileDir, devToolsActivePortFilename), []byte(fmt.Sprintf("%d\n%s", port, browserPath)), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveConfiguredControlURL(context.Background(), autoCDPEndpoint, profileDir, false)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Hostname() != "::1" || parsed.Port() != strconv.Itoa(port) || parsed.Path != browserPath {
		t.Fatalf("resolved IPv6 URL = %q", got)
	}
}

func TestReadDevToolsActivePortRejectsMalformedContents(t *testing.T) {
	for _, content := range []string{
		"9222\n",
		"not-a-port\n/devtools/browser/id\n",
		"0\n/devtools/browser/id\n",
		"65536\n/devtools/browser/id\n",
		"9222\nws://example.test/devtools/browser/id\n",
		"9222\n/devtools/page/id\n",
		"9222\n/devtools/browser/id/extra\n",
		"9222\n/devtools/browser/id?token=x\n",
		"9222\n/devtools/browser/id\nunexpected\n",
	} {
		profileDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(profileDir, devToolsActivePortFilename), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readDevToolsActivePort(profileDir); err == nil {
			t.Fatalf("readDevToolsActivePort(%q) unexpectedly succeeded", content)
		}
	}
}

func TestResolveControlURLRemotePolicyAndDowngrade(t *testing.T) {
	if _, err := resolveControlURL(context.Background(), "wss://browser.example/devtools/browser/id"); err == nil || !strings.Contains(err.Error(), "refusing non-loopback") {
		t.Fatalf("remote endpoint without opt-in error = %v", err)
	}
	if got, err := resolveControlURLWithPolicy(context.Background(), "wss://browser.example/devtools/browser/id", true, noRedirectHTTPClient()); err != nil || got == "" {
		t.Fatalf("opted-in WSS endpoint = %q, %v", got, err)
	}
	if _, err := resolveControlURLWithPolicy(context.Background(), "ws://browser.example/devtools/browser/id", true, noRedirectHTTPClient()); err == nil || !strings.Contains(err.Error(), "must use https or wss") {
		t.Fatalf("insecure remote endpoint error = %v", err)
	}

	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/id"}`))
	}))
	defer tlsServer.Close()
	if _, err := resolveControlURLWithPolicy(context.Background(), tlsServer.URL, false, tlsServer.Client()); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("HTTPS-to-WS downgrade error = %v", err)
	}
}

func TestCDPDiscoveryDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://127.0.0.1/devtools/browser/id"}`))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	if _, err := resolveConfiguredControlURL(context.Background(), redirect.URL, t.TempDir(), false); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("redirected discovery error = %v, want redirect rejection", err)
	}
}

func TestRedactedEndpointHidesCredentials(t *testing.T) {
	got := redactedEndpoint("wss://user:password@browser.example/devtools/browser/id?token=secret")
	if got != "wss://browser.example?redacted" {
		t.Fatalf("redacted endpoint = %q", got)
	}
}

func TestAttachErrorDoesNotEchoEndpointSecrets(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session := New(&config.Config{
		CDPURL:     "http://sensitive-user:sensitive-password@127.0.0.1:1/private-token?token=query-secret",
		ProfileDir: `C:\private\profile`,
	})
	err := session.attach(ctx)
	if err == nil {
		t.Fatal("attach unexpectedly succeeded")
	}
	for _, secret := range []string{"sensitive-user", "sensitive-password", "private-token", "query-secret", `C:\private\profile`} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("attach error exposes %q: %v", secret, err)
		}
	}
}

func TestAbortCancelsBrowserLifetimeWithoutClearingHandles(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	session := New(&config.Config{})
	page := new(rod.Page)
	session.Page = page
	session.abort.Store(&sessionAbort{cancel: cancel})
	session.Abort()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Abort did not cancel the browser lifetime")
	}
	if session.Page != page {
		t.Fatal("Abort concurrently cleared session handles")
	}
	if err := session.EnsureContext(context.Background()); !errors.Is(err, ErrSessionAborted) {
		t.Fatalf("EnsureContext after Abort = %v, want ErrSessionAborted", err)
	}
	if err := session.RecoverContext(context.Background()); !errors.Is(err, ErrSessionAborted) {
		t.Fatalf("RecoverContext after Abort = %v, want ErrSessionAborted", err)
	}
}

func TestRotateDebugShotsOnlyRemovesOwnedPrefix(t *testing.T) {
	dir := t.TempDir()
	legacy := "send-failed-123.png"
	if err := os.WriteFile(filepath.Join(dir, legacy), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyStamp := time.Unix(0, 0)
	if err := os.Chtimes(filepath.Join(dir, legacy), legacyStamp, legacyStamp); err != nil {
		t.Fatal(err)
	}
	owned := []string{"chatgpt-mcp-a-1.png", "chatgpt-mcp-a-2.png", "chatgpt-mcp-a-3.png"}
	for i, name := range owned {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(i+1), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "personal.png")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	rotateDebugShots(dir, 2)
	if _, err := os.Stat(filepath.Join(dir, legacy)); !os.IsNotExist(err) {
		t.Fatalf("legacy bridge screenshot was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, owned[0])); !os.IsNotExist(err) {
		t.Fatalf("oldest owned screenshot was not removed: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated PNG was touched: %v", err)
	}
}
