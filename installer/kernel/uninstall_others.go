//go:build !windows

package kernel

func isUninstallMode() bool              { return false }
func CreateUninstaller(dir string) error { _ = dir; return nil }
func runUninstall()                      {}
