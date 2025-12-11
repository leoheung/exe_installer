//go:build !windows

package kernel

// 非 Windows 平台占位实现
func CreateShortcuts(targetExe, workingDir string, meta InstallMeta) error {
	return nil
}
