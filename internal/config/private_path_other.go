//go:build !unix

package config

func validatePrivatePathAncestors(_, _ string) error {
	return nil
}
