package chatgpt

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"chatgpt-mcp/internal/config"
)

func uploadClient(root string) *Client {
	return &Client{cfg: &config.Config{
		UploadsEnabled:      true,
		UploadAllowedRoots:  []string{root},
		UploadMaxFiles:      2,
		UploadMaxFileBytes:  8,
		UploadMaxTotalBytes: 12,
	}}
}

func skipUnavailableUploadSymlink(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("CHATGPT_MCP_REQUIRE_SYMLINK_TESTS") == "1" {
		t.Fatalf("symbolic-link upload test is required but unavailable: %v", err)
	}
	t.Skipf("symlinks unavailable: %v", err)
}

func writeUploadFixture(t *testing.T, path, contents string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("absolute fixture path: %v", err)
	}
	return abs
}

func TestUploadsAreDisabledByDefault(t *testing.T) {
	t.Parallel()

	client := &Client{cfg: &config.Config{}}
	if _, err := client.validateUploadPaths(context.Background(), []string{`C:\secret.txt`}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("validateUploadPaths error = %v, want disabled error", err)
	}
}

func TestUploadsRejectRemoteCDPEndpoint(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := writeUploadFixture(t, filepath.Join(root, "safe.txt"), "safe")
	client := uploadClient(root)
	for _, endpoint := range []string{
		"wss://browser.example/devtools/browser/id",
		"http://localhost.:9222",
	} {
		client.cfg.CDPURL = endpoint
		if _, err := client.validateUploadPaths(context.Background(), []string{path}); err == nil || !strings.Contains(err.Error(), "remote CDP") {
			t.Fatalf("remote-CDP upload error for %q = %v", endpoint, err)
		}
	}
}

func TestUploadAllowsCanonicalFileWithinRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := writeUploadFixture(t, filepath.Join(root, "nested", "answer.md"), "hello")
	got, err := uploadClient(root).validateUploadPaths(context.Background(), []string{path})
	if err != nil {
		t.Fatalf("validateUploadPaths: %v", err)
	}
	if len(got) != 1 || got[0] != path {
		t.Fatalf("validated paths = %v, want [%s]", got, path)
	}
}

func TestUploadRejectsFileOutsideAllowedRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := writeUploadFixture(t, filepath.Join(t.TempDir(), "secret.txt"), "secret")
	if _, err := uploadClient(root).validateUploadPaths(context.Background(), []string{outside}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("validateUploadPaths error = %v, want outside-root error", err)
	}
}

func TestUploadRejectsAllowedRootResolvingToFilesystemRoot(t *testing.T) {
	t.Parallel()

	link := filepath.Join(t.TempDir(), "root-link")
	root := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		root = filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	}
	if err := os.Symlink(root, link); err != nil {
		skipUnavailableUploadSymlink(t, err)
	}
	if _, err := uploadClient(link).validateUploadPaths(context.Background(), []string{filepath.Join(link, "not-used")}); err == nil || !strings.Contains(err.Error(), "filesystem root") {
		t.Fatalf("filesystem-root upload error = %v", err)
	}
}

func TestUploadRejectsRelativePath(t *testing.T) {
	t.Parallel()

	if _, err := uploadClient(t.TempDir()).validateUploadPaths(context.Background(), []string{"relative.txt"}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("validateUploadPaths error = %v, want absolute-path error", err)
	}
}

func TestUploadEnforcesCountAndSizeLimits(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	first := writeUploadFixture(t, filepath.Join(root, "first.txt"), "1234567")
	second := writeUploadFixture(t, filepath.Join(root, "second.txt"), "123456")
	third := writeUploadFixture(t, filepath.Join(root, "third.txt"), "x")
	client := uploadClient(root)

	if _, err := client.validateUploadPaths(context.Background(), []string{first, second, third}); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("count-limit error = %v", err)
	}
	if _, err := client.validateUploadPaths(context.Background(), []string{first, second}); err == nil || !strings.Contains(err.Error(), "total") {
		t.Fatalf("total-size error = %v", err)
	}

	oversize := writeUploadFixture(t, filepath.Join(root, "oversize.txt"), "123456789")
	if _, err := client.validateUploadPaths(context.Background(), []string{oversize}); err == nil || !strings.Contains(err.Error(), "per-file") {
		t.Fatalf("per-file-size error = %v", err)
	}
}

func TestUploadRejectsDuplicateCanonicalPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := writeUploadFixture(t, filepath.Join(root, "same.txt"), "hello")
	if _, err := uploadClient(root).validateUploadPaths(context.Background(), []string{path, path}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestUploadRejectsDuplicateDisplayNames(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	first := writeUploadFixture(t, filepath.Join(root, "one", "same.txt"), "one")
	second := writeUploadFixture(t, filepath.Join(root, "two", "same.txt"), "two")
	if _, err := uploadClient(root).validateUploadPaths(context.Background(), []string{first, second}); err == nil || !strings.Contains(err.Error(), "filename") {
		t.Fatalf("duplicate filename error = %v", err)
	}
}

func TestPrepareUploadPathsStagesPrivateSnapshots(t *testing.T) {
	t.Parallel()
	if !secureUploadSupported {
		t.Skip("secure upload opening is not supported on this platform")
	}

	root := t.TempDir()
	source := writeUploadFixture(t, filepath.Join(root, "answer.md"), "original")
	client := uploadClient(root)
	staged, names, cleanup, err := client.prepareUploadPaths(context.Background(), []string{source})
	if err != nil {
		t.Fatalf("prepareUploadPaths: %v", err)
	}
	defer cleanup()
	if len(staged) != 1 || len(names) != 1 || names[0] != "answer.md" {
		t.Fatalf("staged=%v names=%v", staged, names)
	}
	if staged[0] == source || filepath.Base(staged[0]) != filepath.Base(source) {
		t.Fatalf("staged path %q did not preserve a private basename snapshot", staged[0])
	}
	if err := os.WriteFile(source, []byte("changed"), 0o600); err != nil {
		t.Fatalf("change source after staging: %v", err)
	}
	contents, err := os.ReadFile(staged[0])
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	if string(contents) != "original" {
		t.Fatalf("staged contents = %q, want immutable original snapshot", contents)
	}
	stagingRoot := filepath.Dir(filepath.Dir(staged[0]))
	cleanup()
	if _, err := os.Stat(stagingRoot); !os.IsNotExist(err) {
		t.Fatalf("staging directory still exists after cleanup: %v", err)
	}
}

func TestSecureUploadOpenReadsValidatedNestedFile(t *testing.T) {
	if !secureUploadSupported {
		t.Skip("secure upload opening is not supported on this platform")
	}

	root := t.TempDir()
	source := writeUploadFixture(t, filepath.Join(root, "nested", "answer.md"), "hello")
	plan, err := uploadClient(root).validateUploadPlan(context.Background(), []string{source})
	if err != nil {
		t.Fatalf("validate upload plan: %v", err)
	}
	secureRoots, err := openSecureUploadRoots(plan.roots)
	if err != nil {
		t.Fatalf("open secure roots: %v", err)
	}
	defer secureRoots.Close()

	file, err := secureRoots.Open(plan.sources[0])
	if err != nil {
		t.Fatalf("securely open nested source: %v", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read securely opened source: %v", err)
	}
	if string(contents) != "hello" {
		t.Fatalf("securely opened contents = %q, want hello", contents)
	}
}

func TestSecureUploadOpenRejectsSymlinkSwapAfterValidation(t *testing.T) {
	if !secureUploadSupported {
		t.Skip("secure upload opening is not supported on this platform")
	}

	root := t.TempDir()
	source := writeUploadFixture(t, filepath.Join(root, "answer.md"), "safe")
	outside := writeUploadFixture(t, filepath.Join(t.TempDir(), "answer.md"), "secret")
	plan, err := uploadClient(root).validateUploadPlan(context.Background(), []string{source})
	if err != nil {
		t.Fatalf("validate upload plan: %v", err)
	}
	if err := os.Rename(source, source+".original"); err != nil {
		t.Fatalf("move validated source: %v", err)
	}
	if err := os.Symlink(outside, source); err != nil {
		skipUnavailableUploadSymlink(t, err)
	}

	secureRoots, err := openSecureUploadRoots(plan.roots)
	if err != nil {
		t.Fatalf("open secure roots: %v", err)
	}
	defer secureRoots.Close()
	file, err := secureRoots.Open(plan.sources[0])
	if file != nil {
		_ = file.Close()
	}
	if err == nil {
		t.Fatal("secure upload open followed a post-validation symlink outside the allowed root")
	}
}

func TestSecureUploadOpenRejectsFileReplacementAfterValidation(t *testing.T) {
	if !secureUploadSupported {
		t.Skip("secure upload opening is not supported on this platform")
	}

	root := t.TempDir()
	source := writeUploadFixture(t, filepath.Join(root, "answer.md"), "safe")
	plan, err := uploadClient(root).validateUploadPlan(context.Background(), []string{source})
	if err != nil {
		t.Fatalf("validate upload plan: %v", err)
	}
	if err := os.Rename(source, source+".original"); err != nil {
		t.Fatalf("move validated source: %v", err)
	}
	writeUploadFixture(t, source, "other")

	secureRoots, err := openSecureUploadRoots(plan.roots)
	if err != nil {
		t.Fatalf("open secure roots: %v", err)
	}
	defer secureRoots.Close()
	file, err := secureRoots.Open(plan.sources[0])
	if file != nil {
		_ = file.Close()
	}
	if err == nil {
		t.Fatal("secure upload open accepted a post-validation file replacement")
	}
}

func TestSecureUploadOpenRejectsRootSwapAfterValidation(t *testing.T) {
	if !secureUploadSupported {
		t.Skip("secure upload opening is not supported on this platform")
	}

	parent := t.TempDir()
	root := filepath.Join(parent, "allowed")
	source := writeUploadFixture(t, filepath.Join(root, "answer.md"), "safe")
	plan, err := uploadClient(root).validateUploadPlan(context.Background(), []string{source})
	if err != nil {
		t.Fatalf("validate upload plan: %v", err)
	}
	movedRoot := filepath.Join(parent, "allowed-original")
	if err := os.Rename(root, movedRoot); err != nil {
		t.Fatalf("move validated root: %v", err)
	}
	writeUploadFixture(t, filepath.Join(root, "answer.md"), "secret")

	secureRoots, err := openSecureUploadRoots(plan.roots)
	if secureRoots != nil {
		secureRoots.Close()
	}
	if err == nil {
		t.Fatal("secure upload roots accepted a post-validation root replacement")
	}
}

func TestUploadValidationHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := uploadClient(t.TempDir()).validateUploadPaths(ctx, []string{"unused"}); err == nil {
		t.Fatal("validateUploadPaths ignored cancellation")
	}
}

func TestUploadRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := writeUploadFixture(t, filepath.Join(t.TempDir(), "secret.txt"), "secret")
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		skipUnavailableUploadSymlink(t, err)
	}

	if _, err := uploadClient(root).validateUploadPaths(context.Background(), []string{link}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("symlink-escape error = %v, want outside-root error", err)
	}
}
