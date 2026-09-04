package chatgpt

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const uploadStagingPrefix = "chatgpt-mcp-upload-"

var staleUploadCleanupOnce sync.Once

type validatedUploadRoot struct {
	path string
	info os.FileInfo
}

type validatedUploadSource struct {
	path      string
	relative  string
	rootIndex int
	info      os.FileInfo
}

type validatedUploadPlan struct {
	roots   []validatedUploadRoot
	sources []validatedUploadSource
}

func cleanupStaleUploadSnapshots() {
	staleUploadCleanupOnce.Do(func() {
		entries, err := os.ReadDir(os.TempDir())
		if err != nil {
			return
		}
		cutoff := time.Now().Add(-24 * time.Hour)
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), uploadStagingPrefix) {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			path := filepath.Join(os.TempDir(), entry.Name())
			if err := os.RemoveAll(path); err != nil {
				log.Printf("warning: could not remove stale private upload snapshot %q: %v", path, err)
			}
		}
	})
}

func (c *Client) validateUploadPaths(ctx context.Context, paths []string) ([]string, error) {
	plan, err := c.validateUploadPlan(ctx, paths)
	if err != nil {
		return nil, err
	}
	validated := make([]string, 0, len(plan.sources))
	for _, source := range plan.sources {
		validated = append(validated, source.path)
	}
	return validated, nil
}

func (c *Client) validateUploadPlan(ctx context.Context, paths []string) (*validatedUploadPlan, error) {
	if !c.cfg.UploadsEnabled {
		return nil, fmt.Errorf("file uploads are disabled; set CHATGPT_UPLOAD_ENABLED=true and configure CHATGPT_UPLOAD_ALLOWED_ROOTS to opt in")
	}
	if strings.TrimSpace(c.cfg.CDPURL) != "" {
		return nil, fmt.Errorf("file uploads require a bridge-launched local browser; unset CHATGPT_CDP_URL because an attached or tunneled browser's filesystem identity cannot be verified")
	}
	if len(c.cfg.UploadAllowedRoots) == 0 {
		return nil, fmt.Errorf("file uploads require at least one CHATGPT_UPLOAD_ALLOWED_ROOTS entry")
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no files given")
	}
	if c.cfg.UploadMaxFiles <= 0 {
		return nil, fmt.Errorf("file uploads are disabled by CHATGPT_UPLOAD_MAX_FILES=%d", c.cfg.UploadMaxFiles)
	}
	if len(paths) > c.cfg.UploadMaxFiles {
		return nil, fmt.Errorf("too many files: got %d, maximum is %d", len(paths), c.cfg.UploadMaxFiles)
	}
	if c.cfg.UploadMaxFileBytes <= 0 || c.cfg.UploadMaxTotalBytes <= 0 {
		return nil, fmt.Errorf("upload byte limits must be positive")
	}

	roots := make([]validatedUploadRoot, 0, len(c.cfg.UploadAllowedRoots))
	for _, configuredRoot := range c.cfg.UploadAllowedRoots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !filepath.IsAbs(configuredRoot) {
			return nil, fmt.Errorf("upload root must be absolute: %q", configuredRoot)
		}
		if isNetworkPath(configuredRoot) {
			return nil, fmt.Errorf("network/device upload roots are not supported: %q", configuredRoot)
		}
		root, err := canonicalPath(configuredRoot)
		if err != nil {
			return nil, fmt.Errorf("invalid upload root %q: %w", configuredRoot, err)
		}
		if filepath.Dir(root) == root {
			return nil, fmt.Errorf("upload root %q resolves to a filesystem root", configuredRoot)
		}
		if isNetworkPath(root) {
			return nil, fmt.Errorf("upload root %q resolves to a network/device path", configuredRoot)
		}
		info, err := secureUploadRootInfo(root)
		if err != nil {
			return nil, fmt.Errorf("inspect upload root %q: %w", configuredRoot, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("upload root %q is not a directory", configuredRoot)
		}
		roots = append(roots, validatedUploadRoot{path: root, info: info})
	}
	secureRoots, err := openSecureUploadRoots(roots)
	if err != nil {
		return nil, fmt.Errorf("secure upload roots: %w", err)
	}
	defer secureRoots.Close()

	validated := make([]validatedUploadSource, 0, len(paths))
	seenPaths := make(map[string]struct{}, len(paths))
	seenNames := make(map[string]struct{}, len(paths))
	var total int64
	for _, requestedPath := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !filepath.IsAbs(requestedPath) {
			return nil, fmt.Errorf("upload path must be absolute: %q", requestedPath)
		}
		if isNetworkPath(requestedPath) {
			return nil, fmt.Errorf("network/device upload paths are not supported: %q", requestedPath)
		}
		path, err := canonicalPath(requestedPath)
		if err != nil {
			return nil, fmt.Errorf("invalid upload path %q: %w", requestedPath, err)
		}
		if isNetworkPath(path) {
			return nil, fmt.Errorf("upload path %q resolves to a network/device path", requestedPath)
		}
		rootIndex, relative := containingUploadRoot(path, roots)
		if rootIndex < 0 {
			return nil, fmt.Errorf("upload path %q resolves outside the configured allowed roots", requestedPath)
		}
		key := canonicalKey(path)
		if _, exists := seenPaths[key]; exists {
			return nil, fmt.Errorf("duplicate upload path %q", requestedPath)
		}
		seenPaths[key] = struct{}{}
		nameKey := strings.ToLower(filepath.Base(path))
		if _, exists := seenNames[nameKey]; exists {
			return nil, fmt.Errorf("duplicate upload filename %q cannot be verified safely", filepath.Base(path))
		}
		seenNames[nameKey] = struct{}{}

		source, err := secureRoots.openRelative(rootIndex, relative)
		if err != nil {
			return nil, fmt.Errorf("securely open upload path %q: %w", requestedPath, err)
		}
		info, statErr := source.Stat()
		closeErr := source.Close()
		if statErr != nil {
			return nil, fmt.Errorf("inspect securely opened upload path %q: %w", requestedPath, statErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close securely opened upload path %q: %w", requestedPath, closeErr)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("upload path %q is not a regular file", requestedPath)
		}
		if info.Size() > c.cfg.UploadMaxFileBytes {
			return nil, fmt.Errorf("upload path %q is %d bytes; per-file maximum is %d", requestedPath, info.Size(), c.cfg.UploadMaxFileBytes)
		}
		if info.Size() > c.cfg.UploadMaxTotalBytes-total {
			return nil, fmt.Errorf("upload total exceeds the maximum of %d bytes", c.cfg.UploadMaxTotalBytes)
		}
		total += info.Size()
		validated = append(validated, validatedUploadSource{
			path:      path,
			relative:  relative,
			rootIndex: rootIndex,
			info:      info,
		})
	}
	return &validatedUploadPlan{roots: roots, sources: validated}, nil
}

// prepareUploadPaths copies securely opened, size-limited source files into a
// private staging directory. Chrome receives only those immutable snapshots,
// narrowing the validation-to-use race and preserving each original basename.
func (c *Client) prepareUploadPaths(ctx context.Context, paths []string) ([]string, []string, func(), error) {
	plan, err := c.validateUploadPlan(ctx, paths)
	if err != nil {
		return nil, nil, func() {}, err
	}
	secureRoots, err := openSecureUploadRoots(plan.roots)
	if err != nil {
		return nil, nil, func() {}, fmt.Errorf("secure upload roots: %w", err)
	}
	defer secureRoots.Close()
	stagingRoot, err := os.MkdirTemp("", uploadStagingPrefix)
	if err != nil {
		return nil, nil, func() {}, fmt.Errorf("create private upload staging directory: %w", err)
	}
	_ = os.Chmod(stagingRoot, 0o700)
	cleanup := func() {
		// Chromium may briefly retain a Windows file handle after SetFiles.
		// Retry so private snapshots do not linger merely because cleanup raced
		// with the browser releasing that handle.
		var lastErr error
		for attempt := 0; attempt < 10; attempt++ {
			if err := os.RemoveAll(stagingRoot); err == nil {
				return
			} else {
				lastErr = err
			}
			time.Sleep(100 * time.Millisecond)
		}
		log.Printf("warning: private upload snapshot %q could not be removed immediately: %v; background cleanup will retry", stagingRoot, lastErr)
		go func() {
			for attempt := 0; attempt < 150; attempt++ {
				time.Sleep(2 * time.Second)
				if err := os.RemoveAll(stagingRoot); err == nil {
					return
				}
			}
			log.Printf("warning: private upload snapshot remains at %q; remove it manually after browser handles are released", stagingRoot)
		}()
	}
	fail := func(err error) ([]string, []string, func(), error) {
		cleanup()
		return nil, nil, func() {}, err
	}

	staged := make([]string, 0, len(plan.sources))
	names := make([]string, 0, len(plan.sources))
	var total int64
	for index, sourceSpec := range plan.sources {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		source, err := secureRoots.Open(sourceSpec)
		if err != nil {
			return fail(fmt.Errorf("securely open upload source %q: %w", sourceSpec.path, err))
		}
		info, statErr := source.Stat()
		if statErr != nil {
			_ = source.Close()
			return fail(fmt.Errorf("inspect opened upload source %q: %w", sourceSpec.path, statErr))
		}
		if !info.Mode().IsRegular() || info.Size() > c.cfg.UploadMaxFileBytes || info.Size() > c.cfg.UploadMaxTotalBytes-total {
			_ = source.Close()
			return fail(fmt.Errorf("upload source %q changed after validation", sourceSpec.path))
		}

		itemDir := filepath.Join(stagingRoot, fmt.Sprintf("%03d", index))
		if err := os.Mkdir(itemDir, 0o700); err != nil {
			_ = source.Close()
			return fail(fmt.Errorf("create upload staging item: %w", err))
		}
		name := filepath.Base(sourceSpec.path)
		destinationPath := filepath.Join(itemDir, name)
		destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = source.Close()
			return fail(fmt.Errorf("create staged upload %q: %w", name, err))
		}
		limit := c.cfg.UploadMaxFileBytes
		if limit < 1<<63-1 {
			limit++
		}
		copied, copyErr := io.Copy(destination, io.LimitReader(&contextReader{ctx: ctx, reader: source}, limit))
		closeDestinationErr := destination.Close()
		closeSourceErr := source.Close()
		if copyErr != nil {
			return fail(fmt.Errorf("stage upload source %q: %w", sourceSpec.path, copyErr))
		}
		if closeDestinationErr != nil || closeSourceErr != nil {
			return fail(fmt.Errorf("close staged upload source %q", sourceSpec.path))
		}
		if copied != info.Size() || copied > c.cfg.UploadMaxFileBytes || copied > c.cfg.UploadMaxTotalBytes-total {
			return fail(fmt.Errorf("upload source %q changed size while staging", sourceSpec.path))
		}
		total += copied
		staged = append(staged, destinationPath)
		names = append(names, name)
	}
	return staged, names, cleanup, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func canonicalKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func isNetworkPath(path string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	// Accept only ordinary drive-letter volumes. UNC paths, Win32 device paths
	// (\\.\ and \\?\), Root Local Device paths (\??\), volume GUIDs, and
	// root-relative device namespaces must all fail closed. filepath.VolumeName
	// recognizes these forms even when they use mixed separators.
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' {
		return true
	}
	letter := volume[0]
	return (letter < 'A' || letter > 'Z') && (letter < 'a' || letter > 'z')
}

func containingUploadRoot(path string, roots []validatedUploadRoot) (int, string) {
	for index, root := range roots {
		relative, err := filepath.Rel(root.path, path)
		if err != nil || relative == "." || filepath.IsAbs(relative) {
			continue
		}
		if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return index, relative
		}
	}
	return -1, ""
}
