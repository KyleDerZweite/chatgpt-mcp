package browser

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"

	"chatgpt-mcp/internal/config"
)

const (
	ChatURL                    = "https://chatgpt.com"
	ModelURLBase               = ChatURL + "/?model="
	loginMaxWait               = 5 * time.Minute
	autoCDPEndpoint            = "auto"
	devToolsActivePortFilename = "DevToolsActivePort"
	maxActivePortFileSize      = 4096
	autoCDPDiscoveryTimeout    = 3 * time.Second
	autoCDPProbeTimeout        = 500 * time.Millisecond
	autoCDPRetryDelay          = 75 * time.Millisecond
)

var loopbackCDPHTTPClient = newLoopbackCDPHTTPClient()

var PromptTextareaSelectors = []string{
	"#prompt-textarea",
	`[data-testid="prompt-textarea"]`,
	`form textarea[placeholder*="Message"]`,
	`form .ProseMirror[contenteditable="true"]`,
	`form div[contenteditable="true"]`,
	`[data-testid*="composer" i] textarea[placeholder*="Message"]`,
	`[data-testid*="composer" i] .ProseMirror[contenteditable="true"]`,
	`[data-testid*="composer" i] div[contenteditable="true"]`,
}

var loginButtonSelectors = []string{
	`button[data-testid="login-button"]`,
	`a[href*="auth/login"]`,
	`button:has-text("Log in")`,
}

var legacyDebugShotName = regexp.MustCompile(`^(?:not-ready|send-failed|snapshot-failed)-[0-9]+\.png$`)

// ErrUntrustedChatGPTOrigin indicates that a page is not the exact trusted origin.
var ErrUntrustedChatGPTOrigin = errors.New("untrusted browser origin: expected https://chatgpt.com")

// ErrSessionAborted indicates that service shutdown permanently stopped this
// session. A fresh process is required before browser automation may resume.
var ErrSessionAborted = errors.New("browser session was aborted during service shutdown")

type Session struct {
	cfg       *config.Config
	rod       *rod.Browser
	launcher  *launcher.Launcher
	transport *cdp.WebSocket
	cancel    context.CancelFunc
	owned     bool
	pageOwned bool
	Page      *rod.Page
	abort     atomic.Pointer[sessionAbort]
	aborted   atomic.Bool
}

type sessionAbort struct {
	cancel    context.CancelFunc
	transport *cdp.WebSocket
}

func New(cfg *config.Config) *Session {
	if cfg != nil && cfg.Screenshots {
		rotateDebugShots(cfg.DebugDir, cfg.DebugMaxFiles)
	}
	return &Session{cfg: cfg}
}

func (s *Session) Ensure() error {
	return s.EnsureContext(context.Background())
}

func (s *Session) EnsureContext(ctx context.Context) error {
	if s.aborted.Load() {
		return ErrSessionAborted
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("ensure browser session: %w", err)
	}
	if s.Page != nil {
		// Ordinary operations remain bound to the selected target. Silently
		// switching to another trusted tab would make Reply mutate the wrong
		// conversation. Only explicit recovery may discard this reference.
		return s.AssertChatGPTOrigin(ctx)
	}

	if err := s.ensureBrowser(ctx); err != nil {
		return err
	}

	targets, err := (proto.TargetGetTargets{}).Call(s.rod.Context(ctx))
	if err != nil {
		return fmt.Errorf("list pages: %w", err)
	}
	trustedTargets := make([]*proto.TargetTargetInfo, 0, 1)
	for _, target := range targets.TargetInfos {
		if target == nil || target.Type != proto.TargetTargetInfoTypePage || ValidateChatGPTURL(target.URL) != nil {
			continue
		}
		trustedTargets = append(trustedTargets, target)
	}
	if len(trustedTargets) > 1 {
		return fmt.Errorf("multiple ChatGPT tabs are available; close all but the intended tab before retrying")
	}
	if len(trustedTargets) == 1 {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("attach to ChatGPT page: %w", err)
		}
		page, err := s.rod.PageFromTarget(trustedTargets[0].TargetID)
		if err != nil {
			return fmt.Errorf("attach to the unique ChatGPT page: %w", err)
		}
		s.Page = page
		s.pageOwned = false
		if err := s.AssertChatGPTOrigin(ctx); err != nil {
			s.Page = nil
			return err
		}
		return nil
	}
	return s.createTrustedPage(ctx)
}

func (s *Session) ensureBrowser(ctx context.Context) error {
	if s.aborted.Load() {
		return ErrSessionAborted
	}
	if s.rod != nil {
		if _, err := (proto.TargetGetTargets{}).Call(s.rod.Context(ctx)); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
		s.discard()
	}
	if s.cfg.CDPURL != "" {
		return s.attach(ctx)
	}
	return s.launch(ctx)
}

func (s *Session) createTrustedPage(ctx context.Context) error {
	page, err := s.newPageContext(ctx, ChatURL)
	if err != nil {
		return fmt.Errorf("new page: %w", err)
	}
	if err := page.Context(ctx).WaitLoad(); err != nil {
		s.closeTarget(page.TargetID)
		return fmt.Errorf("load chatgpt: %w", err)
	}
	s.Page = page
	s.pageOwned = true
	if err := s.AssertChatGPTOrigin(ctx); err != nil {
		s.Page = nil
		s.pageOwned = false
		s.closeTarget(page.TargetID)
		return err
	}
	return nil
}

// RecoverContext explicitly reacquires a trusted target for NewChat. Unlike
// EnsureContext, it may discard a closed or redirected cached page because the
// caller has requested a fresh conversation rather than continuation.
func (s *Session) RecoverContext(ctx context.Context) error {
	if s.aborted.Load() {
		return ErrSessionAborted
	}
	if s.Page != nil {
		if err := s.AssertChatGPTOrigin(ctx); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return err
		}
		page := s.Page
		pageOwned := s.pageOwned
		s.Page = nil
		s.pageOwned = false
		if pageOwned {
			s.closeTarget(page.TargetID)
		}
	}
	if err := s.ensureBrowser(ctx); err != nil {
		return err
	}
	return s.createTrustedPage(ctx)
}

func (s *Session) newPageContext(ctx context.Context, destination string) (page *rod.Page, err error) {
	target, err := (proto.TargetCreateTarget{
		URL:              "about:blank",
		BrowserContextID: s.rod.BrowserContextID,
	}).Call(s.rod.Context(ctx))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			s.closeTarget(target.TargetID)
		}
	}()

	// PageFromTarget uses the session-lifetime browser context so the cached
	// page and its event subscriptions survive the request that created it.
	page, err = s.rod.PageFromTarget(target.TargetID)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = page.Context(ctx).Navigate(destination); err != nil {
		return nil, err
	}
	return page, nil
}

func (s *Session) closeTarget(targetID proto.TargetTargetID) {
	if s.rod == nil {
		return
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = (proto.TargetCloseTarget{TargetID: targetID}).Call(s.rod.Context(closeCtx))
}

// ValidateChatGPTURL verifies that rawURL has the exact ChatGPT web origin.
// Credentials, explicit ports, and subdomains are deliberately rejected.
func ValidateChatGPTURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ErrUntrustedChatGPTOrigin
	}
	if !strings.EqualFold(u.Scheme, "https") ||
		!strings.EqualFold(u.Host, "chatgpt.com") ||
		u.User != nil {
		return ErrUntrustedChatGPTOrigin
	}
	return nil
}

// AssertChatGPTOrigin checks the page's current URL immediately before a
// sensitive operation. Callers should invoke it before typing prompts, setting
// upload files, reading answers, or taking any other action on Session.Page.
func (s *Session) AssertChatGPTOrigin(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("inspect browser page origin: %w", err)
	}
	if s.Page == nil {
		return fmt.Errorf("no browser page is attached")
	}
	pageURL, err := currentPageURL(ctx, s.Page)
	if err != nil {
		return fmt.Errorf("inspect browser page origin: %w", err)
	}
	if err := ValidateChatGPTURL(pageURL); err != nil {
		return err
	}
	return nil
}

func currentPageURL(ctx context.Context, page *rod.Page) (string, error) {
	if page == nil || page.Browser() == nil {
		return "", fmt.Errorf("browser page is not connected")
	}
	res, err := (proto.TargetGetTargetInfo{TargetID: page.TargetID}).Call(page.Browser().Context(ctx))
	if err != nil {
		return "", err
	}
	if res.TargetInfo == nil {
		return "", fmt.Errorf("browser returned no target information")
	}
	return res.TargetInfo.URL, nil
}

func (s *Session) launch(ctx context.Context) error {
	if s.aborted.Load() {
		return ErrSessionAborted
	}
	l := launcher.New().
		Context(ctx).
		UserDataDir(s.cfg.ProfileDir).
		Headless(s.cfg.Headless).
		Leakless(true).
		Set("disable-blink-features", "AutomationControlled").
		Delete("enable-automation")
	if s.cfg.ChromeBin != "" {
		l.Bin(s.cfg.ChromeBin)
	}

	ctrlURL, err := l.Launch()
	if err != nil {
		return fmt.Errorf("start chrome with profile %s: %w (log into ChatGPT in this profile to keep the session)", s.cfg.ProfileDir, err)
	}

	controlURL, err := resolveControlURL(ctx, ctrlURL)
	if err != nil {
		l.Kill()
		return fmt.Errorf("resolve launched Chrome endpoint: %w", safeEndpointCause(err))
	}
	connected, browserCancel, transport, err := connect(ctx, controlURL)
	if err != nil {
		l.Kill()
		return fmt.Errorf("connect to chrome: %w", safeEndpointCause(err))
	}
	abortState := &sessionAbort{cancel: browserCancel, transport: transport}
	s.abort.Store(abortState)
	if s.aborted.Load() {
		browserCancel()
		_ = transport.Close()
		l.Kill()
		s.abort.CompareAndSwap(abortState, nil)
		return ErrSessionAborted
	}
	s.rod = connected
	s.launcher = l
	s.transport = transport
	s.cancel = browserCancel
	s.owned = true
	if s.aborted.Load() {
		s.clearHandles()
		browserCancel()
		_ = transport.Close()
		l.Kill()
		return ErrSessionAborted
	}
	return nil
}

func (s *Session) attach(ctx context.Context) error {
	if s.aborted.Load() {
		return ErrSessionAborted
	}
	controlURL, err := resolveConfiguredControlURL(ctx, s.cfg.CDPURL, s.cfg.ProfileDir, s.cfg.CDPAllowRemote)
	if err != nil {
		return fmt.Errorf("resolve Chrome CDP endpoint %s: %w", redactedEndpoint(s.cfg.CDPURL), safeEndpointCause(err))
	}
	connected, browserCancel, transport, err := connect(ctx, controlURL)
	if err != nil {
		return fmt.Errorf("attach to chrome at %s: %w (start Chrome with remote debugging enabled and log into ChatGPT)", redactedEndpoint(s.cfg.CDPURL), safeEndpointCause(err))
	}
	abortState := &sessionAbort{cancel: browserCancel, transport: transport}
	s.abort.Store(abortState)
	if s.aborted.Load() {
		browserCancel()
		_ = transport.Close()
		s.abort.CompareAndSwap(abortState, nil)
		return ErrSessionAborted
	}
	s.rod = connected
	s.launcher = nil
	s.transport = transport
	s.cancel = browserCancel
	s.owned = false
	if s.aborted.Load() {
		s.clearHandles()
		browserCancel()
		_ = transport.Close()
		return ErrSessionAborted
	}
	return nil
}

func redactedEndpoint(raw string) string {
	if isAutoCDPEndpoint(raw) {
		return autoCDPEndpoint
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "configured endpoint"
	}
	parsed.User = nil
	parsed.Path = ""
	parsed.RawPath = ""
	parsed.Fragment = ""
	if parsed.RawQuery != "" {
		parsed.RawQuery = "redacted"
	}
	return parsed.String()
}

func isAutoCDPEndpoint(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), autoCDPEndpoint)
}

// IsRemoteCDPEndpoint reports whether an explicit configuration refers to a
// non-loopback host. Invalid explicit values are treated as remote so callers
// such as local-file upload fail closed before browser path interpretation.
func IsRemoteCDPEndpoint(raw string) bool {
	value := strings.TrimSpace(raw)
	if value == "" || isAutoCDPEndpoint(value) {
		return false
	}
	if port := strings.TrimPrefix(value, ":"); port != "" && !strings.Contains(port, ":") {
		if _, err := strconv.ParseUint(port, 10, 16); err == nil {
			value = "127.0.0.1:" + port
		}
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	return err != nil || parsed.Host == "" || !isLoopbackCDPHost(parsed.Hostname())
}

func resolveConfiguredControlURL(ctx context.Context, raw, profileDir string, allowRemote bool) (string, error) {
	if isAutoCDPEndpoint(raw) {
		return resolveAutoControlURL(ctx, profileDir)
	}
	return resolveControlURLWithPolicy(ctx, raw, allowRemote, noRedirectHTTPClient())
}

type activePortInfo struct {
	port        uint16
	browserPath string
}

func resolveAutoControlURL(ctx context.Context, profileDir string) (string, error) {
	discoveryCtx, cancel := context.WithTimeout(ctx, autoCDPDiscoveryTimeout)
	defer cancel()
	var lastErr error
	for {
		active, activePortPath, err := readDevToolsActivePort(profileDir)
		if err == nil {
			controlURL, probeErr := probeAutoControlEndpoints(discoveryCtx, active)
			if probeErr == nil {
				current, _, rereadErr := readDevToolsActivePort(profileDir)
				switch {
				case rereadErr != nil:
					err = fmt.Errorf("re-read %s after discovery: %w", filepath.Base(activePortPath), rereadErr)
				case current != active:
					err = fmt.Errorf("%s changed during discovery", filepath.Base(activePortPath))
				default:
					return controlURL, nil
				}
			} else {
				err = fmt.Errorf("no live matching loopback CDP endpoint: %w", probeErr)
			}
		}
		lastErr = err

		timer := time.NewTimer(autoCDPRetryDelay)
		select {
		case <-discoveryCtx.Done():
			timer.Stop()
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return "", fmt.Errorf("automatic CDP discovery timed out after %s: %w", autoCDPDiscoveryTimeout, lastErr)
		case <-timer.C:
		}
	}
}

func probeAutoControlEndpoints(ctx context.Context, active activePortInfo) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, autoCDPProbeTimeout)
	defer cancel()
	type result struct {
		endpoint string
		url      string
		err      error
	}
	results := make(chan result, 2)
	for _, host := range []string{"127.0.0.1", "::1"} {
		endpoint := "http://" + net.JoinHostPort(host, strconv.Itoa(int(active.port)))
		go func(endpoint string) {
			controlURL, resolveErr := resolveAutoEndpoint(probeCtx, endpoint, active)
			results <- result{endpoint: endpoint, url: controlURL, err: resolveErr}
		}(endpoint)
	}

	errList := make([]error, 0, 2)
	for range 2 {
		resolved := <-results
		if resolved.err == nil {
			cancel()
			return resolved.url, nil
		}
		errList = append(errList, fmt.Errorf("%s: %w", resolved.endpoint, resolved.err))
	}
	if err := probeCtx.Err(); err != nil {
		errList = append(errList, err)
	}
	return "", errors.Join(errList...)
}

func resolveAutoEndpoint(ctx context.Context, endpoint string, active activePortInfo) (string, error) {
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	websocketURL, err := requestWebSocketDebuggerURL(ctx, parsedEndpoint, loopbackCDPHTTPClient)
	if err != nil {
		return "", err
	}
	if err := validateAutoWebSocketURL(websocketURL, active); err != nil {
		return "", err
	}
	websocketURL.Host = parsedEndpoint.Host
	return websocketURL.String(), nil
}

func readDevToolsActivePort(profileDir string) (activePortInfo, string, error) {
	if strings.TrimSpace(profileDir) == "" {
		return activePortInfo{}, "", fmt.Errorf("CHATGPT_MCP_DIR is empty")
	}
	activePortPath := filepath.Join(profileDir, devToolsActivePortFilename)
	file, err := os.Open(activePortPath)
	if err != nil {
		return activePortInfo{}, activePortPath, fmt.Errorf("read %s: %w", devToolsActivePortFilename, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxActivePortFileSize+1))
	if err != nil {
		return activePortInfo{}, activePortPath, fmt.Errorf("read %s: %w", devToolsActivePortFilename, err)
	}
	if len(data) > maxActivePortFileSize {
		return activePortInfo{}, activePortPath, fmt.Errorf("%s is unexpectedly large", devToolsActivePortFilename)
	}
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	normalized = strings.TrimSuffix(normalized, "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) != 2 {
		return activePortInfo{}, activePortPath, fmt.Errorf("%s must contain a port and browser target path", devToolsActivePortFilename)
	}
	if lines[0] == "" || strings.IndexFunc(lines[0], func(char rune) bool { return char < '0' || char > '9' }) >= 0 {
		return activePortInfo{}, activePortPath, fmt.Errorf("%s contains an invalid port", devToolsActivePortFilename)
	}
	port, err := strconv.ParseUint(lines[0], 10, 16)
	if err != nil || port == 0 {
		return activePortInfo{}, activePortPath, fmt.Errorf("%s contains an invalid port", devToolsActivePortFilename)
	}
	browserPath := lines[1]
	parsedPath, err := url.Parse(browserPath)
	if err != nil || parsedPath.Scheme != "" || parsedPath.Opaque != "" || parsedPath.User != nil || parsedPath.Host != "" ||
		parsedPath.RawPath != "" || parsedPath.ForceQuery || parsedPath.RawQuery != "" || parsedPath.Fragment != "" ||
		parsedPath.Path != browserPath || !validBrowserTargetPath(browserPath) {
		return activePortInfo{}, activePortPath, fmt.Errorf("%s contains an invalid browser target path", devToolsActivePortFilename)
	}
	return activePortInfo{port: uint16(port), browserPath: parsedPath.Path}, activePortPath, nil
}

func validBrowserTargetPath(path string) bool {
	if path == "/devtools/browser" {
		return true
	}
	const prefix = "/devtools/browser/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	token := strings.TrimPrefix(path, prefix)
	if token == "" {
		return false
	}
	for _, char := range token {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') ||
			char == '-' || char == '.' || char == '_' || char == '~' {
			continue
		}
		return false
	}
	return true
}

func validateAutoWebSocketURL(parsed *url.URL, active activePortInfo) error {
	if parsed.Scheme != "ws" || parsed.Opaque != "" || parsed.User != nil || parsed.RawPath != "" ||
		parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("CDP discovery advertised an invalid local WebSocket URL")
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("CDP discovery advertised a non-loopback WebSocket endpoint")
	}
	if parsed.Port() != strconv.Itoa(int(active.port)) {
		return fmt.Errorf("CDP discovery advertised a different port than %s", devToolsActivePortFilename)
	}
	if parsed.Path != active.browserPath {
		return fmt.Errorf("CDP discovery advertised a different browser target than %s", devToolsActivePortFilename)
	}
	return nil
}

func newLoopbackCDPHTTPClient() *http.Client {
	client, err := loopbackOnlyHTTPClient(http.DefaultClient)
	if err != nil {
		// The standard library's default client always uses *http.Transport.
		// Keep this as an initialization invariant rather than silently falling
		// back to an unvalidated discovery client.
		panic(err)
	}
	return client
}

// safeEndpointCause retains cancellation semantics without allowing an HTTP or
// WebSocket client error to echo credentials, query values, or path tokens from
// the configured endpoint.
func safeEndpointCause(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return errors.New("endpoint connection failed")
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return errors.New("endpoint returned invalid discovery data")
	}
	return errors.New("endpoint operation failed")
}

func resolveControlURL(ctx context.Context, raw string) (string, error) {
	return resolveControlURLWithPolicy(ctx, raw, false, noRedirectHTTPClient())
}

func resolveControlURLWithPolicy(ctx context.Context, raw string, allowRemote bool, client *http.Client) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("endpoint is empty")
	}
	if port := strings.TrimPrefix(value, ":"); port != "" && !strings.Contains(port, ":") {
		allDigits := true
		for _, char := range port {
			if char < '0' || char > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			value = "127.0.0.1:" + port
		}
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if parsed.Host == "" || parsed.Opaque != "" {
		return "", fmt.Errorf("endpoint must use http, https, ws, or wss")
	}
	if err := validateConfiguredCDPEndpoint(parsed, allowRemote); err != nil {
		return "", err
	}
	if parsed.Scheme == "ws" || parsed.Scheme == "wss" {
		return parsed.String(), nil
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("endpoint must use http, https, ws, or wss")
	}
	if isLoopbackCDPHost(parsed.Hostname()) {
		client, err = loopbackOnlyHTTPClient(client)
		if err != nil {
			return "", err
		}
	}
	websocketURL, err := requestWebSocketDebuggerURL(ctx, parsed, client)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "https" && websocketURL.Scheme != "wss" {
		return "", fmt.Errorf("secure CDP discovery attempted to downgrade to an unencrypted WebSocket")
	}
	if !isLoopbackCDPHost(parsed.Hostname()) && websocketURL.Scheme != "wss" {
		return "", fmt.Errorf("remote CDP discovery must advertise a secure WebSocket endpoint")
	}
	websocketURL.Host = parsed.Host
	return websocketURL.String(), nil
}

func validateConfiguredCDPEndpoint(endpoint *url.URL, allowRemote bool) error {
	if isLoopbackCDPHost(endpoint.Hostname()) {
		return nil
	}
	if !allowRemote {
		return fmt.Errorf("refusing non-loopback CDP endpoint; set CHATGPT_CDP_ALLOW_REMOTE=true only for a trusted TLS endpoint")
	}
	if endpoint.Scheme != "https" && endpoint.Scheme != "wss" {
		return fmt.Errorf("remote CDP endpoint must use https or wss")
	}
	return nil
}

func isLoopbackCDPHost(host string) bool {
	// Do not accept the DNS absolute-name spelling "localhost." here.
	// net/http's proxy bypass special case is the exact name "localhost";
	// treating the trailing-dot spelling as local could therefore send CDP
	// credentials and discovery queries through an HTTP proxy.
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func requestWebSocketDebuggerURL(ctx context.Context, endpoint *url.URL, client *http.Client) (*url.URL, error) {
	discovery := *endpoint
	discovery.Path = "/json/version"
	discovery.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discovery.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("CDP discovery returned %s", response.Status)
	}
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&version); err != nil {
		return nil, fmt.Errorf("decode CDP discovery response: %w", err)
	}
	websocketURL, err := url.Parse(version.WebSocketDebuggerURL)
	if err != nil || (websocketURL.Scheme != "ws" && websocketURL.Scheme != "wss") || websocketURL.Host == "" {
		return nil, fmt.Errorf("CDP discovery returned an invalid WebSocket URL")
	}
	return websocketURL, nil
}

func noRedirectHTTPClient() *http.Client {
	return &http.Client{
		Transport: http.DefaultTransport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// loopbackOnlyHTTPClient clones a standard HTTP client for local CDP
// discovery. It bypasses environment proxies and verifies the address reached
// by the dial before net/http can send credentials, query values, or headers.
func loopbackOnlyHTTPClient(base *http.Client) (*http.Client, error) {
	if base == nil {
		base = http.DefaultClient
	}
	baseTransport := base.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	transport, ok := baseTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("loopback CDP discovery requires a standard HTTP transport")
	}
	transport = transport.Clone()
	transport.Proxy = nil

	dialContext := transport.DialContext
	if dialContext == nil {
		dialer := new(net.Dialer)
		dialContext = dialer.DialContext
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := dialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		remote, ok := connection.RemoteAddr().(*net.TCPAddr)
		if !ok || remote.IP == nil || !remote.IP.IsLoopback() {
			_ = connection.Close()
			return nil, fmt.Errorf("CDP discovery resolved outside loopback")
		}
		return connection, nil
	}
	// Force HTTPS through the validated DialContext above. A custom TLS dialer
	// could otherwise bypass the address check; TLSClientConfig is retained by
	// Transport.Clone and is applied after the validated TCP connection opens.
	transport.DialTLS = nil
	transport.DialTLSContext = nil

	restricted := *base
	restricted.Transport = transport
	restricted.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &restricted, nil
}

func connect(requestCtx context.Context, controlURL string) (*rod.Browser, context.CancelFunc, *cdp.WebSocket, error) {
	endpoint, err := url.Parse(controlURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "ws" && endpoint.Scheme != "wss") {
		return nil, nil, nil, fmt.Errorf("invalid WebSocket endpoint")
	}
	dialer, normalizedURL, err := newCDPWebSocketDialer(endpoint)
	if err != nil {
		return nil, nil, nil, err
	}
	transport := &cdp.WebSocket{Dialer: dialer}
	if err := transport.Connect(requestCtx, normalizedURL, nil); err != nil {
		dialer.stopSetup()
		if dialer.connection != nil {
			_ = transport.Close()
		}
		if requestErr := requestCtx.Err(); requestErr != nil {
			return nil, nil, nil, requestErr
		}
		return nil, nil, nil, err
	}
	browserCtx, browserCancel := context.WithCancel(context.Background())
	connected := rod.New().Client(cdp.New().Start(transport)).Context(browserCtx)
	stopCancellation := context.AfterFunc(requestCtx, func() {
		browserCancel()
		_ = transport.Close()
	})
	if err := dialer.finishSetup(requestCtx); err != nil {
		stopCancellation()
		browserCancel()
		_ = transport.Close()
		return nil, nil, nil, err
	}
	if err := connected.Connect(); err != nil {
		stopCancellation()
		browserCancel()
		_ = transport.Close()
		return nil, nil, nil, err
	}
	if !stopCancellation() || requestCtx.Err() != nil || browserCtx.Err() != nil {
		browserCancel()
		_ = transport.Close()
		if err := requestCtx.Err(); err != nil {
			return nil, nil, nil, err
		}
		return nil, nil, nil, context.Canceled
	}
	return connected, browserCancel, transport, nil
}

// cdpWebSocketDialer owns only the TCP/TLS setup phase. Rod's WebSocket
// implementation performs the HTTP upgrade after DialContext returns, so the
// dialer keeps the request context armed against the socket until connect has
// observed a successful upgrade. This makes a peer that accepts TCP but never
// answers the upgrade request cancellation-aware as well.
type cdpWebSocketDialer struct {
	scheme          string
	requireLoopback bool
	dialContext     func(context.Context, string, string) (net.Conn, error)
	tlsConfig       *tls.Config

	connection net.Conn
	stop       func() bool
	stopDone   <-chan struct{}
}

func newCDPWebSocketDialer(endpoint *url.URL) (*cdpWebSocketDialer, string, error) {
	if endpoint == nil || endpoint.Host == "" || (endpoint.Scheme != "ws" && endpoint.Scheme != "wss") {
		return nil, "", fmt.Errorf("endpoint must use ws or wss")
	}
	host := endpoint.Hostname()
	if host == "" {
		return nil, "", fmt.Errorf("WebSocket endpoint has no host")
	}
	port := endpoint.Port()
	if port == "" {
		if endpoint.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	normalized := *endpoint
	normalized.Host = net.JoinHostPort(host, port)
	networkDialer := new(net.Dialer)
	dialer := &cdpWebSocketDialer{
		scheme:          endpoint.Scheme,
		requireLoopback: isLoopbackCDPHost(host),
		dialContext:     networkDialer.DialContext,
		tlsConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: host,
		},
	}
	return dialer, normalized.String(), nil
}

func (d *cdpWebSocketDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	connection, err := d.dialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if err := validateCDPWebSocketPeer(connection, d.requireLoopback); err != nil {
		_ = connection.Close()
		return nil, err
	}
	if d.scheme == "wss" {
		tlsConnection := tls.Client(connection, d.tlsConfig.Clone())
		if err := tlsConnection.HandshakeContext(ctx); err != nil {
			_ = connection.Close()
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			return nil, err
		}
		connection = tlsConnection
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetDeadline(deadline); err != nil {
			_ = connection.Close()
			return nil, err
		}
	}
	stopDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = connection.Close()
		close(stopDone)
	})
	d.connection = connection
	d.stop = stop
	d.stopDone = stopDone
	return connection, nil
}

func validateCDPWebSocketPeer(connection net.Conn, requireLoopback bool) error {
	if !requireLoopback {
		return nil
	}
	remote, ok := connection.RemoteAddr().(*net.TCPAddr)
	if !ok || remote.IP == nil || !remote.IP.IsLoopback() {
		return fmt.Errorf("CDP WebSocket resolved outside loopback")
	}
	return nil
}

func (d *cdpWebSocketDialer) finishSetup(ctx context.Context) error {
	if d.stop == nil || d.connection == nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("WebSocket dialer did not establish a connection")
	}
	if !d.stop() {
		<-d.stopDone
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.connection.SetDeadline(time.Time{})
}

func (d *cdpWebSocketDialer) stopSetup() {
	if d.stop == nil {
		return
	}
	if !d.stop() {
		<-d.stopDone
	}
}

func (s *Session) Navigate(url string) error {
	return s.NavigateContext(context.Background(), url)
}

func (s *Session) NavigateContext(ctx context.Context, url string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("navigate ChatGPT: %w", err)
	}
	if err := ValidateChatGPTURL(url); err != nil {
		return err
	}
	if s.Page == nil {
		return fmt.Errorf("navigate: no browser page is attached")
	}
	page := s.Page.Context(ctx)
	if err := page.Navigate(url); err != nil {
		return fmt.Errorf("navigate to %s: %w", url, err)
	}
	if err := page.WaitLoad(); err != nil {
		return fmt.Errorf("load %s: %w", url, err)
	}
	return s.AssertChatGPTOrigin(ctx)
}

func (s *Session) Ready() bool {
	return s.ReadyContext(context.Background())
}

func (s *Session) ReadyContext(ctx context.Context) bool {
	if s.AssertChatGPTOrigin(ctx) != nil {
		return false
	}
	return s.hasAny(ctx, PromptTextareaSelectors)
}

func (s *Session) NeedsLogin() bool {
	return s.NeedsLoginContext(context.Background())
}

func (s *Session) NeedsLoginContext(ctx context.Context) bool {
	if s.AssertChatGPTOrigin(ctx) != nil {
		return false
	}
	return s.hasAny(ctx, loginButtonSelectors)
}

func (s *Session) hasAny(ctx context.Context, selectors []string) bool {
	page := s.Page.Context(ctx)
	for _, sel := range selectors {
		if found, _, err := page.Has(sel); err == nil && found {
			return true
		}
	}
	return false
}

func (s *Session) WaitReady(max time.Duration) error {
	return s.WaitReadyContext(context.Background(), max)
}

func (s *Session) WaitReadyContext(ctx context.Context, max time.Duration) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("wait for chat interface: %w", err)
	}

	timer := time.NewTimer(max)
	defer timer.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	loginShown := false
	for {
		if err := s.AssertChatGPTOrigin(ctx); err != nil {
			return err
		}
		if s.hasAny(ctx, PromptTextareaSelectors) {
			return nil
		}
		loginShown = s.hasAny(ctx, loginButtonSelectors)

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for chat interface: %w", ctx.Err())
		case <-timer.C:
			if loginShown {
				return fmt.Errorf("ChatGPT shows a login prompt; log in using the browser window and retry")
			}
			return fmt.Errorf("chat interface did not become ready within %s", max)
		case <-ticker.C:
		}
	}
}

func (s *Session) DebugShot(name string) {
	s.DebugShotContext(context.Background(), name)
}

func (s *Session) DebugShotContext(_ context.Context, name string) {
	if !s.cfg.Screenshots || s.Page == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.AssertChatGPTOrigin(ctx) != nil {
		return
	}
	data, err := s.Page.Context(ctx).Screenshot(false, nil)
	if err != nil {
		return
	}
	path := filepath.Join(s.cfg.DebugDir, fmt.Sprintf("chatgpt-mcp-%s-%d.png", name, time.Now().UnixNano()))
	if err := os.WriteFile(path, data, 0o600); err == nil {
		_ = os.Chmod(path, 0o600)
		rotateDebugShots(s.cfg.DebugDir, s.cfg.DebugMaxFiles)
	}
}

// Close shuts down Chrome only when this session launched it. Attached CDP
// sessions are owned by their caller; only a bridge-owned tab is closed.
func (s *Session) Close() error {
	if s.rod == nil {
		return nil
	}
	connected := s.rod
	launcherProcess := s.launcher
	transport := s.transport
	cancelBrowser := s.cancel
	owned := s.owned
	page := s.Page
	pageOwned := s.pageOwned
	s.clearHandles()

	if !owned {
		var closeErr error
		if pageOwned && page != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, closeErr = (proto.TargetCloseTarget{TargetID: page.TargetID}).Call(connected.Context(ctx))
			cancel()
		}
		if cancelBrowser != nil {
			cancelBrowser()
		}
		if transport != nil {
			_ = transport.Close()
		}
		return closeErr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := connected.Context(ctx).Close()
	cancel()
	if cancelBrowser != nil {
		cancelBrowser()
	}
	if transport != nil {
		_ = transport.Close()
	}
	if err != nil && launcherProcess != nil {
		launcherProcess.Kill()
	}
	return err
}

// Invalidate discards an automation connection after a state-reset failure.
// An attached external Chrome process is left running.
func (s *Session) Invalidate() {
	s.discard()
}

// Abort interrupts in-flight CDP calls without concurrently mutating session
// handles. Normal cleanup can safely run after the browser operation unwinds.
func (s *Session) Abort() {
	s.aborted.Store(true)
	state := s.abort.Load()
	if state == nil {
		return
	}
	if state.cancel != nil {
		state.cancel()
	}
	if state.transport != nil {
		_ = state.transport.Close()
	}
}

func (s *Session) discard() {
	connected := s.rod
	launcherProcess := s.launcher
	transport := s.transport
	cancelBrowser := s.cancel
	owned := s.owned
	page := s.Page
	pageOwned := s.pageOwned
	s.clearHandles()

	if connected == nil {
		if cancelBrowser != nil {
			cancelBrowser()
		}
		if transport != nil {
			_ = transport.Close()
		}
		return
	}
	if !owned {
		if pageOwned && page != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = (proto.TargetCloseTarget{TargetID: page.TargetID}).Call(connected.Context(ctx))
			cancel()
		}
		if cancelBrowser != nil {
			cancelBrowser()
		}
		if transport != nil {
			_ = transport.Close()
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := connected.Context(ctx).Close()
	cancel()
	if cancelBrowser != nil {
		cancelBrowser()
	}
	if transport != nil {
		_ = transport.Close()
	}
	if err != nil && launcherProcess != nil {
		launcherProcess.Kill()
	}
}

func (s *Session) clearHandles() {
	s.abort.Store(nil)
	s.Page = nil
	s.rod = nil
	s.launcher = nil
	s.transport = nil
	s.cancel = nil
	s.owned = false
	s.pageOwned = false
}

func rotateDebugShots(dir string, keep int) {
	if keep <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type shot struct {
		path    string
		modTime time.Time
	}
	shots := make([]shot, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !ownedDebugShotName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err == nil {
			shots = append(shots, shot{path: filepath.Join(dir, entry.Name()), modTime: info.ModTime()})
		}
	}
	if len(shots) <= keep {
		return
	}
	sort.Slice(shots, func(i, j int) bool { return shots[i].modTime.Before(shots[j].modTime) })
	for _, old := range shots[:len(shots)-keep] {
		_ = os.Remove(old.path)
	}
}

func ownedDebugShotName(name string) bool {
	return (strings.HasPrefix(name, "chatgpt-mcp-") && strings.HasSuffix(name, ".png")) || legacyDebugShotName.MatchString(name)
}
