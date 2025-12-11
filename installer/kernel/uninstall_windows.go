//go:build windows

package kernel

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// 判断当前是否为卸载模式：可执行文件名包含 "uninstall"。
func isUninstallMode() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	name := strings.ToLower(filepath.Base(exe))
	return strings.Contains(name, "uninstall")
}

// 在导入时检查是否为卸载器自身运行，如果是则执行卸载逻辑并退出。
func init() {
	if isUninstallMode() {
		runUninstall()
		os.Exit(0)
	}
}

// CreateUninstaller: 在 installDir 下生成一个 uninstall.exe（复制当前可执行）。
func CreateUninstaller(installDir string) error {
	if installDir == "" {
		return fmt.Errorf("installDir is empty")
	}
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}

	dst := filepath.Join(installDir, "uninstall.exe")
	// 先尝试删除旧文件
	_ = os.Remove(dst)

	// 复制当前可执行到 uninstall.exe
	if err := copyFile(self, dst); err != nil {
		return fmt.Errorf("copy self -> uninstall.exe failed: %w", err)
	}
	// 确保可执行权限
	_ = os.Chmod(dst, 0755)

	return nil
}

// runUninstall：仅清空安装目录内容（保留目录本身），退出后由批处理自删除 exe 并删除目录。
func runUninstall() {
	fmt.Println("正在卸载...")
	// 这里简单：通过可执行所在目录上一级推断安装根目录。
	exe, _ := os.Executable()
	installDir := filepath.Dir(exe)

	entries, _ := os.ReadDir(installDir)
	for _, e := range entries {
		p := filepath.Join(installDir, e.Name())
		// 保留自身，稍后批处理删除
		if strings.EqualFold(p, exe) {
			continue
		}
		_ = os.RemoveAll(p)
	}

	// 计划自删除与目录删除
	if err := scheduleSelfDelete(exe, installDir); err != nil {
		fmt.Printf("自删除计划失败（手动删除目录）：%v\n", err)
	} else {
		fmt.Println("已计划删除卸载程序与安装目录...")
	}
	fmt.Println("卸载完成。")
}

// scheduleSelfDelete: 使用临时批处理在当前进程退出后循环尝试删除 exe 与安装目录，最后删除自身批处理。
func scheduleSelfDelete(exePath, installDir string) error {
	tempBat := filepath.Join(os.TempDir(), fmt.Sprintf("_uninst_del_%d.bat", os.Getpid()))
	// 构造批处理：等待1-2秒 -> 删除 exe -> 若仍存在则重试 -> 删除目录 -> 删除批处理
	script := fmt.Sprintf(`@echo off\r
set EXE="%s"\r
set DIR="%s"\r
:again\r
ping -n 2 127.0.0.1 >nul\r
del /f /q %s >nul 2>&1\r
if exist %s goto again\r
rmdir /s /q %s >nul 2>&1\r
del /f /q "%s" >nul 2>&1\r
`, exePath, installDir, exePath, exePath, installDir, tempBat)
	if err := os.WriteFile(tempBat, []byte(script), 0o644); err != nil {
		return err
	}
	// 启动批处理（新窗口/后台）。
	_, _ = os.StartProcess("cmd", []string{"cmd", "/c", "start", "", tempBat}, &os.ProcAttr{Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}})
	return nil
}

// 复制文件（覆盖写）
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, in)
	return err
}
