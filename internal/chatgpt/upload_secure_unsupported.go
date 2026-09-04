//go:build !unix && !windows && !wasip1

package chatgpt

import (
	"fmt"
	"os"
)

const secureUploadSupported = false

// Go documents os.Root as vulnerable to path-swap races on the remaining
// platforms (currently js/wasm and Plan 9). Uploads fail closed there.
type secureUploadRoots struct{}

func secureUploadRootInfo(_ string) (os.FileInfo, error) {
	return nil, fmt.Errorf("secure file upload is not supported on this platform")
}

func openSecureUploadRoots(_ []validatedUploadRoot) (*secureUploadRoots, error) {
	return nil, fmt.Errorf("secure file upload is not supported on this platform")
}

func (roots *secureUploadRoots) Open(_ validatedUploadSource) (*os.File, error) {
	return nil, fmt.Errorf("secure file upload is not supported on this platform")
}

func (roots *secureUploadRoots) openRelative(_ int, _ string) (*os.File, error) {
	return nil, fmt.Errorf("secure file upload is not supported on this platform")
}

func (roots *secureUploadRoots) Close() {}
