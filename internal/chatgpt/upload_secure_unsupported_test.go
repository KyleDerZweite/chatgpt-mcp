//go:build !unix && !windows && !wasip1

package chatgpt

import "testing"

func TestSecureUploadsFailClosedOnUnsupportedPlatform(t *testing.T) {
	if _, err := openSecureUploadRoots(nil); err == nil {
		t.Fatal("secure upload root opening unexpectedly succeeded on an unsupported platform")
	}
}
