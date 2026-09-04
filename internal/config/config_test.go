package config

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

var configEnvironment = []string{
	"CHATGPT_MCP_DIR",
	"CHATGPT_CDP_URL",
	"CHATGPT_CDP_ALLOW_REMOTE",
	"CHATGPT_HEADLESS",
	"CHATGPT_CHROME_BIN",
	"CHATGPT_DELAY_MS",
	"CHATGPT_TIMEOUT_MINUTES",
	"CHATGPT_MAX_TIMEOUT_MINUTES",
	"CHATGPT_DEBUG_DIR",
	"CHATGPT_SCREENSHOTS",
	"CHATGPT_DEBUG_MAX_FILES",
	"CHATGPT_UPLOAD_ENABLED",
	"CHATGPT_UPLOAD_ALLOWED_ROOTS",
	"CHATGPT_UPLOAD_MAX_FILES",
	"CHATGPT_UPLOAD_MAX_FILE_BYTES",
	"CHATGPT_UPLOAD_MAX_TOTAL_BYTES",
	"CHATGPT_PROVIDER_ADDR",
	"CHATGPT_PROVIDER_API_KEY",
	"CHATGPT_PROVIDER_MODELS",
	"CHATGPT_PROVIDER_DEFAULT_MODEL",
	"CHATGPT_PROVIDER_ALLOW_REMOTE",
	"CHATGPT_PROVIDER_TLS_CERT_FILE",
	"CHATGPT_PROVIDER_TLS_KEY_FILE",
}

type statePaths struct {
	profile string
	debug   string
}

func cleanTestEnvironment(t *testing.T) statePaths {
	t.Helper()
	for _, name := range configEnvironment {
		t.Setenv(name, "")
	}
	root := t.TempDir()
	paths := statePaths{
		profile: filepath.Join(root, "profile"),
		debug:   filepath.Join(root, "debug"),
	}
	t.Setenv("CHATGPT_MCP_DIR", paths.profile)
	t.Setenv("CHATGPT_DEBUG_DIR", paths.debug)
	return paths
}

func clearUserHomeEnvironment(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		for _, name := range []string{"USERPROFILE", "HOMEDRIVE", "HOMEPATH"} {
			t.Setenv(name, "")
		}
		return
	}
	if runtime.GOOS == "plan9" {
		t.Setenv("home", "")
		return
	}
	t.Setenv("HOME", "")
}

func mustLoad(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	return cfg
}

func requireLoadError(t *testing.T, contains string) {
	t.Helper()
	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load() = %+v, want error containing %q", cfg, contains)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("Load() error = %q, want it to contain %q", err, contains)
	}
}

func skipUnavailableSymlink(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("CHATGPT_MCP_REQUIRE_SYMLINK_TESTS") == "1" {
		t.Fatalf("symbolic-link test is required but unavailable: %v", err)
	}
	t.Skipf("symbolic links unavailable: %v", err)
}

func TestUploadConfigurationDefaultsToDisabled(t *testing.T) {
	paths := cleanTestEnvironment(t)

	cfg := mustLoad(t)
	if cfg.UploadsEnabled {
		t.Fatal("uploads enabled by default")
	}
	if len(cfg.UploadAllowedRoots) != 0 {
		t.Fatalf("default upload roots = %v, want none", cfg.UploadAllowedRoots)
	}
	if cfg.UploadMaxFiles != 5 || cfg.UploadMaxFileBytes != 25*1024*1024 || cfg.UploadMaxTotalBytes != 50*1024*1024 {
		t.Fatalf("unexpected upload defaults: %+v", cfg)
	}
	if cfg.ProviderTLSCertFile != "" || cfg.ProviderTLSKeyFile != "" {
		t.Fatalf("provider TLS files are configured by default: cert=%q key=%q", cfg.ProviderTLSCertFile, cfg.ProviderTLSKeyFile)
	}
	if _, err := os.Stat(paths.debug); !os.IsNotExist(err) {
		t.Fatalf("disabled screenshot directory stat error = %v, want not-exist", err)
	}
}

func TestExplicitStatePathsDoNotRequireUserHome(t *testing.T) {
	paths := cleanTestEnvironment(t)
	clearUserHomeEnvironment(t)
	t.Setenv("CHATGPT_SCREENSHOTS", "true")

	cfg := mustLoad(t)
	if cfg.ProfileDir != paths.profile || cfg.DebugDir != paths.debug {
		t.Fatalf("state paths = profile %q, debug %q; want %q and %q", cfg.ProfileDir, cfg.DebugDir, paths.profile, paths.debug)
	}
	for _, path := range []string{paths.profile, paths.debug} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("explicit state path %q was not prepared as a directory: info=%v err=%v", path, info, err)
		}
	}
}

func TestUnusedStatePathDefaultsDoNotRequireUserHome(t *testing.T) {
	cleanTestEnvironment(t)
	t.Setenv("CHATGPT_MCP_DIR", "")
	t.Setenv("CHATGPT_DEBUG_DIR", "")
	t.Setenv("CHATGPT_CDP_URL", "ws://127.0.0.1:9222/devtools/browser/id")
	clearUserHomeEnvironment(t)

	cfg := mustLoad(t)
	if cfg.ProfileDir != "" || cfg.DebugDir != "" {
		t.Fatalf("unused state paths = profile %q, debug %q; want both empty", cfg.ProfileDir, cfg.DebugDir)
	}
}

func TestWindowsNetworkAndDevicePathClassification(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path namespaces are Windows-specific")
	}
	local := filepath.Join(t.TempDir(), "child")
	tests := []struct {
		path string
		want bool
	}{
		{path: local, want: false},
		{path: `\\server\share\directory`, want: true},
		{path: `\\?\C:\directory`, want: true},
		{path: `\\.\PhysicalDrive0`, want: true},
		{path: `\??\C:\directory`, want: true},
		{path: `\??\UNC\server\share\directory`, want: true},
		{path: `\??\Volume{00000000-0000-0000-0000-000000000000}\directory`, want: true},
		{path: `\Device\HarddiskVolume1\directory`, want: true},
	}
	for _, test := range tests {
		if got := isNetworkOrDevicePath(test.path); got != test.want {
			t.Errorf("isNetworkOrDevicePath(%q) = %t, want %t", test.path, got, test.want)
		}
	}
}

func TestUploadConfigurationParsesCanonicalAllowedRootsAndLimits(t *testing.T) {
	cleanTestEnvironment(t)
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	for _, root := range []string{first, second} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatalf("create upload root: %v", err)
		}
	}
	firstAlias := filepath.Join(first, "..", filepath.Base(first))
	t.Setenv("CHATGPT_UPLOAD_ENABLED", "true")
	t.Setenv("CHATGPT_UPLOAD_ALLOWED_ROOTS", firstAlias+string(filepath.ListSeparator)+second+string(filepath.ListSeparator)+first)
	t.Setenv("CHATGPT_UPLOAD_MAX_FILES", "3")
	t.Setenv("CHATGPT_UPLOAD_MAX_FILE_BYTES", "100")
	t.Setenv("CHATGPT_UPLOAD_MAX_TOTAL_BYTES", "250")

	cfg := mustLoad(t)
	if !cfg.UploadsEnabled {
		t.Fatal("uploads were not enabled")
	}
	wantFirst, err := canonicalPath(first)
	if err != nil {
		t.Fatal(err)
	}
	wantSecond, err := canonicalPath(second)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.UploadAllowedRoots) != 2 || cfg.UploadAllowedRoots[0] != wantFirst || cfg.UploadAllowedRoots[1] != wantSecond {
		t.Fatalf("upload roots = %v, want [%s %s]", cfg.UploadAllowedRoots, wantFirst, wantSecond)
	}
	if cfg.UploadMaxFiles != 3 || cfg.UploadMaxFileBytes != 100 || cfg.UploadMaxTotalBytes != 250 {
		t.Fatalf("unexpected upload limits: %+v", cfg)
	}
}

func TestInvalidBooleanConfigurationReturnsError(t *testing.T) {
	for _, name := range []string{
		"CHATGPT_HEADLESS",
		"CHATGPT_CDP_ALLOW_REMOTE",
		"CHATGPT_SCREENSHOTS",
		"CHATGPT_UPLOAD_ENABLED",
		"CHATGPT_PROVIDER_ALLOW_REMOTE",
	} {
		t.Run(name, func(t *testing.T) {
			paths := cleanTestEnvironment(t)
			t.Setenv(name, "sometimes")
			requireLoadError(t, name)
			if _, err := os.Stat(paths.profile); !os.IsNotExist(err) {
				t.Fatalf("profile directory stat error = %v, want no state creation after invalid configuration", err)
			}
		})
	}
}

func TestInvalidIntegerConfigurationReturnsError(t *testing.T) {
	maxDurationMinutes := int64(math.MaxInt64 / int64(time.Minute))
	tests := []struct {
		name  string
		value string
	}{
		{name: "CHATGPT_DELAY_MS", value: "-1"},
		{name: "CHATGPT_TIMEOUT_MINUTES", value: "0"},
		{name: "CHATGPT_MAX_TIMEOUT_MINUTES", value: strconv.FormatInt(maxDurationMinutes+1, 10)},
		{name: "CHATGPT_DEBUG_MAX_FILES", value: "0"},
		{name: "CHATGPT_UPLOAD_MAX_FILES", value: "not-a-number"},
		{name: "CHATGPT_UPLOAD_MAX_FILE_BYTES", value: "-10"},
		{name: "CHATGPT_UPLOAD_MAX_TOTAL_BYTES", value: "9223372036854775808"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cleanTestEnvironment(t)
			t.Setenv(test.name, test.value)
			requireLoadError(t, test.name)
		})
	}
}

func TestTimeoutAndUploadRangesMustBeConsistent(t *testing.T) {
	t.Run("default timeout exceeds maximum", func(t *testing.T) {
		cleanTestEnvironment(t)
		t.Setenv("CHATGPT_TIMEOUT_MINUTES", "60")
		t.Setenv("CHATGPT_MAX_TIMEOUT_MINUTES", "10")
		requireLoadError(t, "must not exceed CHATGPT_MAX_TIMEOUT_MINUTES")
	})

	t.Run("per-file upload maximum exceeds total", func(t *testing.T) {
		cleanTestEnvironment(t)
		t.Setenv("CHATGPT_UPLOAD_MAX_FILE_BYTES", "251")
		t.Setenv("CHATGPT_UPLOAD_MAX_TOTAL_BYTES", "250")
		requireLoadError(t, "must not exceed CHATGPT_UPLOAD_MAX_TOTAL_BYTES")
	})
}

func TestProviderConfigurationDefaultsAndLists(t *testing.T) {
	cleanTestEnvironment(t)
	t.Setenv("CHATGPT_PROVIDER_API_KEY", "  local-secret  ")
	t.Setenv("CHATGPT_PROVIDER_MODELS", "chatgpt-auto, gpt-5-pro, chatgpt-auto")
	t.Setenv("CHATGPT_PROVIDER_DEFAULT_MODEL", "gpt-5-pro")
	t.Setenv("CHATGPT_PROVIDER_ALLOW_REMOTE", "true")
	t.Setenv("CHATGPT_PROVIDER_TLS_CERT_FILE", "  server.crt  ")
	t.Setenv("CHATGPT_PROVIDER_TLS_KEY_FILE", "  server.key  ")

	cfg := mustLoad(t)
	if cfg.ProviderAddr != "127.0.0.1:8787" {
		t.Fatalf("provider address = %q", cfg.ProviderAddr)
	}
	if cfg.ProviderAPIKey != "local-secret" || !cfg.ProviderAllowRemote {
		t.Fatalf("unexpected provider security configuration: %+v", cfg)
	}
	if cfg.ProviderTLSCertFile != "server.crt" || cfg.ProviderTLSKeyFile != "server.key" {
		t.Fatalf("unexpected provider TLS configuration: cert=%q key=%q", cfg.ProviderTLSCertFile, cfg.ProviderTLSKeyFile)
	}
	wantModels := []string{"chatgpt-auto", "gpt-5-pro"}
	if len(cfg.ProviderModels) != len(wantModels) || cfg.ProviderModels[0] != wantModels[0] || cfg.ProviderModels[1] != wantModels[1] {
		t.Fatalf("provider models = %v, want %v", cfg.ProviderModels, wantModels)
	}
	if cfg.ProviderDefaultModel != "gpt-5-pro" {
		t.Fatalf("provider default model = %q", cfg.ProviderDefaultModel)
	}
}

func TestProviderTLSConfigurationRequiresCertificateAndKey(t *testing.T) {
	for _, test := range []struct {
		name string
		cert string
		key  string
	}{
		{name: "certificate only", cert: "server.crt"},
		{name: "key only", key: "server.key"},
		{name: "whitespace certificate", cert: "   ", key: "server.key"},
		{name: "whitespace key", cert: "server.crt", key: "   "},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths := cleanTestEnvironment(t)
			t.Setenv("CHATGPT_PROVIDER_TLS_CERT_FILE", test.cert)
			t.Setenv("CHATGPT_PROVIDER_TLS_KEY_FILE", test.key)
			requireLoadError(t, "must be set together")
			if _, err := os.Stat(paths.profile); !os.IsNotExist(err) {
				t.Fatalf("profile directory stat error = %v, want no state creation after invalid TLS configuration", err)
			}
		})
	}
}

func TestProviderModelConfigurationIsValidated(t *testing.T) {
	tests := []struct {
		name         string
		models       string
		defaultModel string
		want         string
	}{
		{name: "empty registry", models: ", ,", defaultModel: "chatgpt-auto", want: "at least one model ID"},
		{name: "invalid leading punctuation", models: ".gpt-5", defaultModel: ".gpt-5", want: "invalid model ID"},
		{name: "invalid trailing punctuation", models: "gpt-5-", defaultModel: "gpt-5-", want: "invalid model ID"},
		{name: "invalid separator", models: "gpt/5", defaultModel: "gpt/5", want: "invalid model ID"},
		{name: "default absent", models: "gpt-5,gpt-5-pro", defaultModel: "chatgpt-auto", want: "is not listed"},
		{name: "invalid blank default", models: "gpt-5", defaultModel: " ", want: "invalid model ID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cleanTestEnvironment(t)
			t.Setenv("CHATGPT_PROVIDER_MODELS", test.models)
			t.Setenv("CHATGPT_PROVIDER_DEFAULT_MODEL", test.defaultModel)
			requireLoadError(t, test.want)
		})
	}
}

func TestEnabledUploadsRequireValidExistingLocalRoots(t *testing.T) {
	tests := []struct {
		name     string
		root     func(*testing.T) string
		contains string
	}{
		{name: "missing", root: func(*testing.T) string { return "" }, contains: "at least one directory"},
		{name: "relative", root: func(*testing.T) string { return "relative-root" }, contains: "must be absolute"},
		{name: "filesystem root", root: func(*testing.T) string {
			if runtime.GOOS == "windows" {
				volume := filepath.VolumeName(t.TempDir())
				return volume + string(filepath.Separator)
			}
			return string(filepath.Separator)
		}, contains: "must not include a filesystem root"},
		{name: "nonexistent", root: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") }, contains: "resolve CHATGPT_UPLOAD_ALLOWED_ROOTS"},
		{name: "regular file", root: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "file.txt")
			if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}, contains: "is not a directory"},
	}
	if runtime.GOOS == "windows" {
		for _, test := range []struct {
			name string
			path string
		}{
			{name: "network path", path: `\\server\share`},
			{name: "root local device drive path", path: `\??\C:\upload`},
			{name: "root local device UNC path", path: `\??\UNC\server\share\upload`},
			{name: "extended volume path", path: `\\?\Volume{00000000-0000-0000-0000-000000000000}\upload`},
		} {
			test := test
			tests = append(tests, struct {
				name     string
				root     func(*testing.T) string
				contains string
			}{name: test.name, root: func(*testing.T) string { return test.path }, contains: "must be local"})
		}
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cleanTestEnvironment(t)
			t.Setenv("CHATGPT_UPLOAD_ENABLED", "true")
			t.Setenv("CHATGPT_UPLOAD_ALLOWED_ROOTS", test.root(t))
			requireLoadError(t, test.contains)
		})
	}
}

func TestUploadRootMustNotResolveToFilesystemRoot(t *testing.T) {
	cleanTestEnvironment(t)
	link := filepath.Join(t.TempDir(), "root-link")
	root := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		root = filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	}
	if err := os.Symlink(root, link); err != nil {
		skipUnavailableSymlink(t, err)
	}
	t.Setenv("CHATGPT_UPLOAD_ENABLED", "true")
	t.Setenv("CHATGPT_UPLOAD_ALLOWED_ROOTS", link)
	requireLoadError(t, "resolves to a filesystem root")
}

func TestDisabledUploadsDoNotValidateDormantRoots(t *testing.T) {
	cleanTestEnvironment(t)
	t.Setenv("CHATGPT_UPLOAD_ENABLED", "false")
	t.Setenv("CHATGPT_UPLOAD_ALLOWED_ROOTS", "relative-missing-root")

	cfg := mustLoad(t)
	if cfg.UploadsEnabled {
		t.Fatal("uploads unexpectedly enabled")
	}
	if len(cfg.UploadAllowedRoots) != 1 || cfg.UploadAllowedRoots[0] != "relative-missing-root" {
		t.Fatalf("dormant upload roots = %v, want the unvalidated configured value", cfg.UploadAllowedRoots)
	}
}

func TestLoadCreatesOnlyEnabledPrivateStateDirectories(t *testing.T) {
	paths := cleanTestEnvironment(t)
	mustLoad(t)

	profileInfo, err := os.Stat(paths.profile)
	if err != nil {
		t.Fatalf("stat profile: %v", err)
	}
	if !profileInfo.IsDir() {
		t.Fatalf("%s was not created as a directory", paths.profile)
	}
	if runtime.GOOS != "windows" && profileInfo.Mode().Perm() != 0o700 {
		t.Fatalf("profile mode = %o, want 700", profileInfo.Mode().Perm())
	}
	if _, err := os.Stat(paths.debug); !os.IsNotExist(err) {
		t.Fatalf("disabled screenshot directory stat error = %v, want not-exist", err)
	}

	t.Setenv("CHATGPT_SCREENSHOTS", "true")
	mustLoad(t)
	debugInfo, err := os.Stat(paths.debug)
	if err != nil {
		t.Fatalf("stat screenshot directory: %v", err)
	}
	if !debugInfo.IsDir() {
		t.Fatalf("%s was not created as a directory", paths.debug)
	}
	if runtime.GOOS != "windows" && debugInfo.Mode().Perm() != 0o700 {
		t.Fatalf("screenshot directory mode = %o, want 700", debugInfo.Mode().Perm())
	}
}

func TestLoadReturnsStateDirectoryErrors(t *testing.T) {
	t.Run("profile", func(t *testing.T) {
		paths := cleanTestEnvironment(t)
		if err := os.WriteFile(paths.profile, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		requireLoadError(t, "browser profile path is not a directory")
	})

	t.Run("enabled screenshots", func(t *testing.T) {
		paths := cleanTestEnvironment(t)
		if err := os.WriteFile(paths.debug, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CHATGPT_SCREENSHOTS", "true")
		requireLoadError(t, "screenshot path is not a directory")
	})
}

func TestExplicitCDPDoesNotCreateOrValidateUnusedProfileDirectory(t *testing.T) {
	paths := cleanTestEnvironment(t)
	if err := os.WriteFile(paths.profile, []byte("not used in explicit attach mode"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHATGPT_CDP_URL", "ws://127.0.0.1:9222/devtools/browser/id")
	if _, err := Load(); err != nil {
		t.Fatalf("Load() rejected unused profile directory: %v", err)
	}
}

func TestStateDirectoriesRejectFilesystemRootsAndSymlinks(t *testing.T) {
	t.Run("filesystem root", func(t *testing.T) {
		cleanTestEnvironment(t)
		root := filepath.VolumeName(filepath.Clean(string(filepath.Separator))) + string(filepath.Separator)
		if runtime.GOOS != "windows" {
			root = string(filepath.Separator)
		}
		t.Setenv("CHATGPT_MCP_DIR", root)
		requireLoadError(t, "refusing to use filesystem root")
	})

	t.Run("symbolic link", func(t *testing.T) {
		paths := cleanTestEnvironment(t)
		target := filepath.Join(t.TempDir(), "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, paths.profile); err != nil {
			skipUnavailableSymlink(t, err)
		}
		requireLoadError(t, "must not contain a symbolic link")
	})

	t.Run("intermediate symbolic link", func(t *testing.T) {
		paths := cleanTestEnvironment(t)
		target := filepath.Join(t.TempDir(), "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(filepath.Dir(paths.profile), "link")
		if err := os.Symlink(target, link); err != nil {
			skipUnavailableSymlink(t, err)
		}
		t.Setenv("CHATGPT_MCP_DIR", filepath.Join(link, "profile"))
		requireLoadError(t, "must not contain a symbolic link")
	})
}

func TestExistingStateDirectoryPermissionsAreNotMutated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode validation is not available on Windows")
	}
	paths := cleanTestEnvironment(t)
	if err := os.Mkdir(paths.profile, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(paths.profile, 0o755); err != nil {
		t.Fatal(err)
	}
	requireLoadError(t, "grants group or other permissions")
	info, err := os.Stat(paths.profile)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("Load mutated existing directory mode to %o", got)
	}
}

func TestStateDirectoryRejectsUntrustedWritableAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ancestor mode validation is not available on Windows")
	}
	cleanTestEnvironment(t)
	ancestor := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(ancestor, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ancestor, 0o777); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(ancestor, "profile")
	if err := os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHATGPT_MCP_DIR", profile)
	requireLoadError(t, "untrusted writable ancestor")
	info, err := os.Stat(ancestor)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o777 {
		t.Fatalf("Load mutated ancestor mode to %o", got)
	}
}
