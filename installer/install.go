package installer

import (
	"exe_installer/installer/kernel"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// 扁平化的安装入口，便于用户直接调用。
// sourcePath: 已下载的主程序路径
// targetDir: 安装目标目录
// productName: 产品名称（用于注册表、显示）
// exeName: 安装后重命名的可执行名（为空则沿用源文件名）
// createDesktop: 是否创建桌面快捷方式
// createStartMenu: 是否创建开始菜单快捷方式
func InstallApp(
	sourcePath string,
	targetDir string,
	productName string,
	exeName string,
	createDesktop bool,
	createStartMenu bool,
) error {
	meta := kernel.InstallMeta{
		ProductName:             productName,
		ExeName:                 exeName,
		InstallDir:              targetDir,
		CreateDesktopShortcut:   createDesktop,
		CreateStartMenuShortcut: createStartMenu,
		ShortcutName:            "", // 可选：留空则使用 ProductName
	}
	opts := installOptions{
		SourcePath: sourcePath,
		TargetDir:  targetDir,
		Meta:       meta,
	}
	return install(opts)
}

// installOptions 定义安装操作的输入
type installOptions struct {
	SourcePath string
	TargetDir  string
	Meta       kernel.InstallMeta
}

// install 执行安装逻辑：将输入文件移动到目标目录、写注册表、创建快捷方式、生成卸载器。
func install(opts installOptions) error {
	if opts.SourcePath == "" {
		return fmt.Errorf("source path is empty")
	}
	if opts.TargetDir == "" {
		return fmt.Errorf("target dir is empty")
	}
	if opts.Meta.ProductName == "" {
		return fmt.Errorf("product name is empty")
	}

	// 准备目标目录
	if err := os.MkdirAll(opts.TargetDir, 0755); err != nil {
		return fmt.Errorf("mkdir target dir: %w", err)
	}

	// 目标文件名：使用 Meta.ExeName（若为空则用源文件名）
	targetName := opts.Meta.ExeName
	if targetName == "" {
		targetName = filepath.Base(opts.SourcePath)
	}
	targetPath := filepath.Join(opts.TargetDir, targetName)

	// 将源文件移动/复制到目标目录
	if err := moveOrCopyFile(opts.SourcePath, targetPath); err != nil {
		return fmt.Errorf("move/copy file: %w")
	}

	// Windows：写注册表
	if runtime.GOOS == "windows" {
		if err := kernel.WriteRegistry(opts.Meta, opts.TargetDir, targetPath); err != nil {
			return fmt.Errorf("write registry: %w", err)
		}
	}

	// 快捷方式（按 flag）
	if (opts.Meta.CreateDesktopShortcut || opts.Meta.CreateStartMenuShortcut) && runtime.GOOS == "windows" {
		if err := kernel.CreateShortcuts(targetPath, opts.TargetDir, opts.Meta); err != nil {
			return fmt.Errorf("create shortcuts: %w", err)
		}
	}

	// 生成卸载器（现在仅清空安装目录内容，Windows/其它平台均可安全 noop）
	if err := createUninstaller(opts.TargetDir); err != nil {
		return fmt.Errorf("create uninstaller: %w", err)
	}

	return nil
}

// createUninstaller 封装内核的实现（当前行为：清空安装目录内容）
func createUninstaller(installDir string) error {
	return kernel.CreateUninstaller(installDir)
}

// moveOrCopyFile 先尝试 os.Rename，失败则复制覆盖。
func moveOrCopyFile(src, dst string) error {
	// 若目标存在，先移除
	_ = os.Remove(dst)

	// 尝试重命名
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	// 回退到复制
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return nil
}

// copyFile 复制文件内容到新路径（覆盖写）
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()

	// 确保父目录存在
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("mkdir dst dir: %w", err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	defer func() {
		_ = out.Close()
	}()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}
