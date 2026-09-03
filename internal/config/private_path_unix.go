//go:build unix

package config

import (
	"fmt"
	"os"
	"path/filepath"
)

func validatePrivatePathAncestors(path, purpose string) error {
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			writableByOthers := info.Mode().Perm()&0o022 != 0
			sticky := info.Mode()&os.ModeSticky != 0
			if writableByOthers && !sticky {
				return fmt.Errorf("%s directory has an untrusted writable ancestor %q (mode %o)", purpose, current, info.Mode().Perm())
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect %s directory ancestor %q: %w", purpose, current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}
