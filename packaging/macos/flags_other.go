//go:build !darwin

package macos

// clearUserFlags does nothing where there are no BSD file flags: the
// uninstall's tests run on Linux too.
func clearUserFlags(string) error { return nil }
