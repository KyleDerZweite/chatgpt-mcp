package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultDelayMs             = 1000
	defaultTimeoutMinutes      = 30
	defaultMaxTimeoutMinutes   = 120
	defaultDebugMaxFiles       = 20
	defaultUploadMaxFiles      = 5
	defaultUploadMaxFileBytes  = int64(25 * 1024 * 1024)
	defaultUploadMaxTotalBytes = int64(50 * 1024 * 1024)
	defaultProviderAddr        = "127.0.0.1:8787"
	defaultProviderModel       = "chatgpt-auto"
)

var providerModelIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,62}[A-Za-z0-9])?$`)

type Config struct {
	ProfileDir            string
	CDPURL                string
	CDPAllowRemote        bool
	Headless              bool
	ChromeBin             string
	DelayMs               int
	DefaultTimeoutMinutes int
	MaxTimeoutMinutes     int
	DebugDir              string
	Screenshots           bool
	DebugMaxFiles         int
	UploadsEnabled        bool
	UploadAllowedRoots    []string
	UploadMaxFiles        int
	UploadMaxFileBytes    int64
	UploadMaxTotalBytes   int64
	ProviderAddr          string
	ProviderAPIKey        string
	ProviderModels        []string
	ProviderDefaultModel  string
	ProviderAllowRemote   bool
	ProviderTLSCertFile   string
	ProviderTLSKeyFile    string
}

func env(name, def string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return def
}

func envBool(name string, def bool) (bool, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return value, nil
}

func envIntInRange(name string, def, min, max int64) (int64, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	if value < min || value > max {
		return 0, fmt.Errorf("%s must be between %d and %d, got %d", name, min, max, value)
	}
	return value, nil
}

func maxIntValue() int64 {
	return int64(^uint(0) >> 1)
}

func envInt(name string, def int, min, max int64) (int, error) {
	if platformMax := maxIntValue(); max > platformMax {
		max = platformMax
	}
	value, err := envIntInRange(name, int64(def), min, max)
	if err != nil {
		return 0, err
	}
	return int(value), nil
}

func envInt64(name string, def, min, max int64) (int64, error) {
	return envIntInRange(name, def, min, max)
}

func envPaths(name string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}

	var paths []string
	for _, path := range filepath.SplitList(raw) {
		if path = strings.TrimSpace(path); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func envList(name, def string) []string {
	parts := strings.Split(env(name, def), ",")
	values := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values
}

func defaultStatePaths() (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("determine user home directory: %w", err)
	}
	stateRoot := filepath.Join(home, ".chatgpt-mcp")
	return filepath.Join(stateRoot, "Profile"), filepath.Join(stateRoot, "debug"), nil
}

func preparePrivateDir(path, purpose string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s directory %q: %w", purpose, path, err)
	}
	absolute = filepath.Clean(absolute)
	if filepath.Dir(absolute) == absolute {
		return "", fmt.Errorf("refusing to use filesystem root %q as the %s directory", absolute, purpose)
	}
	if isNetworkOrDevicePath(absolute) {
		return "", fmt.Errorf("%s directory must be local, not a network/device path: %q", purpose, absolute)
	}
	if err := rejectSymlinkComponents(absolute, purpose); err != nil {
		return "", err
	}
	if err := validatePrivatePathAncestors(absolute, purpose); err != nil {
		return "", err
	}

	info, err := os.Lstat(absolute)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(absolute, 0o700); err != nil {
			return "", fmt.Errorf("create %s directory %q: %w", purpose, absolute, err)
		}
		if err := rejectSymlinkComponents(absolute, purpose); err != nil {
			return "", err
		}
		if err := validatePrivatePathAncestors(absolute, purpose); err != nil {
			return "", err
		}
		info, err = os.Lstat(absolute)
	}
	if err != nil {
		return "", fmt.Errorf("inspect %s directory %q: %w", purpose, absolute, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s directory must not be a symbolic link: %q", purpose, absolute)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s path is not a directory: %q", purpose, absolute)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s directory %q grants group or other permissions (%o); set its mode to 0700", purpose, absolute, info.Mode().Perm())
	}
	return absolute, nil
}

func rejectSymlinkComponents(path, purpose string) error {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s directory path must not contain a symbolic link: %q", purpose, current)
		}
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect %s directory path %q: %w", purpose, current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

func isNetworkOrDevicePath(path string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	normalized := strings.ReplaceAll(path, "/", `\`)
	return strings.HasPrefix(normalized, `\\`)
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func canonicalKey(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func validateUploadRoots(roots []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("CHATGPT_UPLOAD_ALLOWED_ROOTS must contain at least one directory when CHATGPT_UPLOAD_ENABLED=true")
	}

	validated := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, configuredRoot := range roots {
		if !filepath.IsAbs(configuredRoot) {
			return nil, fmt.Errorf("CHATGPT_UPLOAD_ALLOWED_ROOTS entry must be absolute: %q", configuredRoot)
		}
		if isNetworkOrDevicePath(configuredRoot) {
			return nil, fmt.Errorf("CHATGPT_UPLOAD_ALLOWED_ROOTS entry must be local, not a network/device path: %q", configuredRoot)
		}
		cleanRoot := filepath.Clean(configuredRoot)
		if filepath.Dir(cleanRoot) == cleanRoot {
			return nil, fmt.Errorf("CHATGPT_UPLOAD_ALLOWED_ROOTS must not include a filesystem root: %q", configuredRoot)
		}
		root, err := canonicalPath(configuredRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve CHATGPT_UPLOAD_ALLOWED_ROOTS entry %q: %w", configuredRoot, err)
		}
		if filepath.Dir(root) == root {
			return nil, fmt.Errorf("CHATGPT_UPLOAD_ALLOWED_ROOTS entry %q resolves to a filesystem root", configuredRoot)
		}
		if isNetworkOrDevicePath(root) {
			return nil, fmt.Errorf("CHATGPT_UPLOAD_ALLOWED_ROOTS entry %q resolves to a network/device path", configuredRoot)
		}
		info, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("inspect CHATGPT_UPLOAD_ALLOWED_ROOTS entry %q: %w", configuredRoot, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("CHATGPT_UPLOAD_ALLOWED_ROOTS entry is not a directory: %q", configuredRoot)
		}
		key := canonicalKey(root)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		validated = append(validated, root)
	}
	return validated, nil
}

func validateProviderModels(models []string, defaultModel string) error {
	if len(models) == 0 {
		return fmt.Errorf("CHATGPT_PROVIDER_MODELS must contain at least one model ID")
	}
	foundDefault := false
	for _, model := range models {
		if !providerModelIDPattern.MatchString(model) {
			return fmt.Errorf("CHATGPT_PROVIDER_MODELS contains invalid model ID %q", model)
		}
		if model == defaultModel {
			foundDefault = true
		}
	}
	if !providerModelIDPattern.MatchString(defaultModel) {
		return fmt.Errorf("CHATGPT_PROVIDER_DEFAULT_MODEL contains invalid model ID %q", defaultModel)
	}
	if !foundDefault {
		return fmt.Errorf("CHATGPT_PROVIDER_DEFAULT_MODEL %q is not listed in CHATGPT_PROVIDER_MODELS", defaultModel)
	}
	return nil
}

func Load() (*Config, error) {
	defaultProfile, defaultDebug, err := defaultStatePaths()
	if err != nil {
		return nil, err
	}

	headless, err := envBool("CHATGPT_HEADLESS", false)
	if err != nil {
		return nil, err
	}
	cdpAllowRemote, err := envBool("CHATGPT_CDP_ALLOW_REMOTE", false)
	if err != nil {
		return nil, err
	}
	screenshots, err := envBool("CHATGPT_SCREENSHOTS", false)
	if err != nil {
		return nil, err
	}
	uploadsEnabled, err := envBool("CHATGPT_UPLOAD_ENABLED", false)
	if err != nil {
		return nil, err
	}
	providerAllowRemote, err := envBool("CHATGPT_PROVIDER_ALLOW_REMOTE", false)
	if err != nil {
		return nil, err
	}
	providerTLSCertFile := strings.TrimSpace(os.Getenv("CHATGPT_PROVIDER_TLS_CERT_FILE"))
	providerTLSKeyFile := strings.TrimSpace(os.Getenv("CHATGPT_PROVIDER_TLS_KEY_FILE"))
	if (providerTLSCertFile == "") != (providerTLSKeyFile == "") {
		return nil, fmt.Errorf("CHATGPT_PROVIDER_TLS_CERT_FILE and CHATGPT_PROVIDER_TLS_KEY_FILE must be set together")
	}

	maxDelayMs := int64(math.MaxInt64 / int64(time.Millisecond))
	delayMs, err := envInt("CHATGPT_DELAY_MS", defaultDelayMs, 0, maxDelayMs)
	if err != nil {
		return nil, err
	}
	maxDurationMinutes := int64(math.MaxInt64 / int64(time.Minute))
	timeoutMinutes, err := envInt("CHATGPT_TIMEOUT_MINUTES", defaultTimeoutMinutes, 1, maxDurationMinutes)
	if err != nil {
		return nil, err
	}
	maxTimeoutMinutes, err := envInt("CHATGPT_MAX_TIMEOUT_MINUTES", defaultMaxTimeoutMinutes, 1, maxDurationMinutes)
	if err != nil {
		return nil, err
	}
	if timeoutMinutes > maxTimeoutMinutes {
		return nil, fmt.Errorf("CHATGPT_TIMEOUT_MINUTES (%d) must not exceed CHATGPT_MAX_TIMEOUT_MINUTES (%d)", timeoutMinutes, maxTimeoutMinutes)
	}
	debugMaxFiles, err := envInt("CHATGPT_DEBUG_MAX_FILES", defaultDebugMaxFiles, 1, maxIntValue())
	if err != nil {
		return nil, err
	}
	uploadMaxFiles, err := envInt("CHATGPT_UPLOAD_MAX_FILES", defaultUploadMaxFiles, 1, maxIntValue())
	if err != nil {
		return nil, err
	}
	uploadMaxFileBytes, err := envInt64("CHATGPT_UPLOAD_MAX_FILE_BYTES", defaultUploadMaxFileBytes, 1, math.MaxInt64)
	if err != nil {
		return nil, err
	}
	uploadMaxTotalBytes, err := envInt64("CHATGPT_UPLOAD_MAX_TOTAL_BYTES", defaultUploadMaxTotalBytes, 1, math.MaxInt64)
	if err != nil {
		return nil, err
	}
	if uploadMaxFileBytes > uploadMaxTotalBytes {
		return nil, fmt.Errorf("CHATGPT_UPLOAD_MAX_FILE_BYTES (%d) must not exceed CHATGPT_UPLOAD_MAX_TOTAL_BYTES (%d)", uploadMaxFileBytes, uploadMaxTotalBytes)
	}

	uploadRoots := envPaths("CHATGPT_UPLOAD_ALLOWED_ROOTS")
	if uploadsEnabled {
		uploadRoots, err = validateUploadRoots(uploadRoots)
		if err != nil {
			return nil, err
		}
	}

	providerModels := envList("CHATGPT_PROVIDER_MODELS", defaultProviderModel)
	providerDefaultModel := strings.TrimSpace(env("CHATGPT_PROVIDER_DEFAULT_MODEL", defaultProviderModel))
	if err := validateProviderModels(providerModels, providerDefaultModel); err != nil {
		return nil, err
	}

	cfg := &Config{
		ProfileDir:            env("CHATGPT_MCP_DIR", defaultProfile),
		CDPURL:                env("CHATGPT_CDP_URL", ""),
		CDPAllowRemote:        cdpAllowRemote,
		Headless:              headless,
		ChromeBin:             env("CHATGPT_CHROME_BIN", ""),
		DelayMs:               delayMs,
		DefaultTimeoutMinutes: timeoutMinutes,
		MaxTimeoutMinutes:     maxTimeoutMinutes,
		DebugDir:              env("CHATGPT_DEBUG_DIR", defaultDebug),
		Screenshots:           screenshots,
		DebugMaxFiles:         debugMaxFiles,
		UploadsEnabled:        uploadsEnabled,
		UploadAllowedRoots:    uploadRoots,
		UploadMaxFiles:        uploadMaxFiles,
		UploadMaxFileBytes:    uploadMaxFileBytes,
		UploadMaxTotalBytes:   uploadMaxTotalBytes,
		ProviderAddr:          env("CHATGPT_PROVIDER_ADDR", defaultProviderAddr),
		ProviderAPIKey:        strings.TrimSpace(os.Getenv("CHATGPT_PROVIDER_API_KEY")),
		ProviderModels:        providerModels,
		ProviderDefaultModel:  providerDefaultModel,
		ProviderAllowRemote:   providerAllowRemote,
		ProviderTLSCertFile:   providerTLSCertFile,
		ProviderTLSKeyFile:    providerTLSKeyFile,
	}

	if strings.TrimSpace(cfg.CDPURL) == "" || strings.EqualFold(strings.TrimSpace(cfg.CDPURL), "auto") {
		profileDir, err := preparePrivateDir(cfg.ProfileDir, "browser profile")
		if err != nil {
			return nil, err
		}
		cfg.ProfileDir = profileDir
	}
	if cfg.Screenshots {
		debugDir, err := preparePrivateDir(cfg.DebugDir, "screenshot")
		if err != nil {
			return nil, err
		}
		cfg.DebugDir = debugDir
	}
	return cfg, nil
}
