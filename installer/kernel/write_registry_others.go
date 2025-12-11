//go:build !windows

package kernel

// WriteRegistry 在非 Windows 平台为无操作，以保持编译通过。
func WriteRegistry(meta InstallMeta, installDir, exePath string) error {
	_ = meta
	_ = installDir
	_ = exePath
	return nil
}
