//go:build unix

package chatgpt

import (
	"fmt"
	"os"
	"syscall"
)

const secureUploadSupported = true

// secureUploadRoots owns race-resistant directory handles for the configured
// upload roots. os.Root resolves each descendant relative to the held handle
// and rejects symlink traversal outside it.
type secureUploadRoots struct {
	roots []*os.Root
}

func secureUploadRootInfo(path string) (os.FileInfo, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Stat(".")
}

func openSecureUploadRoots(specs []validatedUploadRoot) (*secureUploadRoots, error) {
	opened := &secureUploadRoots{roots: make([]*os.Root, 0, len(specs))}
	for _, spec := range specs {
		root, err := os.OpenRoot(spec.path)
		if err != nil {
			opened.Close()
			return nil, fmt.Errorf("open allowed root %q: %w", spec.path, err)
		}
		info, err := root.Stat(".")
		if err != nil {
			_ = root.Close()
			opened.Close()
			return nil, fmt.Errorf("inspect opened allowed root %q: %w", spec.path, err)
		}
		if !os.SameFile(spec.info, info) {
			_ = root.Close()
			opened.Close()
			return nil, fmt.Errorf("allowed root %q was replaced after validation", spec.path)
		}
		opened.roots = append(opened.roots, root)
	}
	return opened, nil
}

func (roots *secureUploadRoots) Open(source validatedUploadSource) (*os.File, error) {
	file, err := roots.openRelative(source.rootIndex, source.relative)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !os.SameFile(source.info, info) {
		_ = file.Close()
		return nil, fmt.Errorf("upload source was replaced after validation")
	}
	return file, nil
}

func (roots *secureUploadRoots) openRelative(rootIndex int, relative string) (*os.File, error) {
	if rootIndex < 0 || rootIndex >= len(roots.roots) {
		return nil, fmt.Errorf("upload source has no validated allowed root")
	}
	// A hostile replacement with a FIFO must not block validation or staging.
	return roots.roots[rootIndex].OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

func (roots *secureUploadRoots) Close() {
	if roots == nil {
		return
	}
	for _, root := range roots.roots {
		_ = root.Close()
	}
}
