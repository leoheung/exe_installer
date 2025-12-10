package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	magicTrailer = "SFXMAGIC"
	trailerSize  = 8 + 8
)

// InstallMeta 与打包时的 meta.json 对应
type InstallMeta struct {
	ProductName             string `json:"productName"`
	ExeName                 string `json:"exeName"`
	InstallDir              string `json:"installDir"`
	CreateDesktopShortcut   bool   `json:"createDesktopShortcut"`
	CreateStartMenuShortcut bool   `json:"createStartMenuShortcut"`
	Version                 string `json:"version"`
	GeneratedAt             string `json:"generatedAt"`
	ShortcutName            string `json:"shortcutName"`
}

// 默认值（若 meta.json 缺失）
var meta = InstallMeta{
	ProductName:             "MyApp",
	ExeName:                 "app.exe",
	InstallDir:              "",
	CreateDesktopShortcut:   true,
	CreateStartMenuShortcut: true,
	ShortcutName:            "", // 为空表示使用 ProductName
}

type inMemoryFile struct {
	Name string
	Mode int64
	Data []byte
}

func main() {
	if isUninstallMode() {
		runUninstall()
		return
	}

	fmt.Println("正在安装，请稍候...")

	archive, err := extractSelf()
	if err != nil {
		fmt.Printf("无法提取内置归档: %v\n", err)
		_ = pressAnyKey()
		return
	}

	fmt.Println("正在解压归档...")
	files, err := untarGzToMemory(archive)
	if err != nil {
		fmt.Printf("解包归档失败: %v\n", err)
		_ = pressAnyKey()
		return
	}
	fmt.Printf("解压完成，共 %d 个条目。\n", len(files))

	// 解析 meta.json
	if m := findFile(files, "meta.json"); m != nil {
		_ = json.Unmarshal(m.Data, &meta) // 宽松处理
	}
	fmt.Printf("产品: %s  版本: %s\n", meta.ProductName, meta.Version)

	installDir, err := decideInstallDir(meta.ProductName, meta.InstallDir)
	if err != nil {
		fmt.Printf("创建安装目录失败: %v\n", err)
		_ = pressAnyKey()
		return
	}
	fmt.Printf("目标安装目录: %s\n", installDir)

	// 在写入之前清理旧内容（保留目录本身），避免残留旧版本文件
	fmt.Println("清理旧版本文件（若存在）...")
	if err := cleanInstallDir(installDir); err != nil {
		fmt.Printf("清理已有目录失败: %v\n", err)
		_ = pressAnyKey()
		return
	}
	fmt.Println("目录清理完成，开始写入文件...")

	if err := writeFilesWithLog(files, installDir); err != nil {
		fmt.Printf("写文件失败: %v\n", err)
		_ = pressAnyKey()
		return
	}
	fmt.Println("文件写入完成。")

	fmt.Printf("已安装到: %s\n", installDir)

	// 确定实际 exe 路径
	exePath := filepath.Join(installDir, meta.ExeName)
	if _, err := os.Stat(exePath); err != nil {
		fmt.Printf("未找到指定主程序 %s，尝试自动查找...\n", meta.ExeName)
		if detected := detectAnyExe(installDir); detected != "" {
			fmt.Printf("自动发现可执行文件: %s\n", detected)
			exePath = detected
		} else {
			fmt.Println("未发现任何 .exe，跳过快捷方式创建。")
			_ = pressAnyKey()
			return
		}
	}

	if runtime.GOOS == "windows" && (meta.CreateDesktopShortcut || meta.CreateStartMenuShortcut) {
		fmt.Println("开始创建快捷方式...")
		if err := createShortcuts(exePath, installDir, meta); err != nil {
			fmt.Printf("创建快捷方式失败（忽略）：%v\n", err)
		} else {
			fmt.Println("快捷方式创建完成。")
		}
	}

	// 生成卸载程序并写入注册表（仅 Windows 生效）
	if runtime.GOOS == "windows" {
		if err := createUninstaller(installDir); err != nil {
			fmt.Printf("创建卸载程序失败（忽略）：%v\n", err)
		}
		if err := writeRegistry(meta, installDir, exePath); err != nil {
			fmt.Printf("写入注册表失败（忽略）：%v\n", err)
		} else {
			fmt.Println("已写入注册表信息。")
		}
	}

	fmt.Println("安装完成，祝您使用愉快！")
	_ = pressAnyKey()
}

func pressAnyKey() error {
	fmt.Print("按回车退出...")
	_, err := fmt.Scanln()
	return err
}

// ========== 归档解包到内存 ==========

func untarGzToMemory(gzData []byte) ([]*inMemoryFile, error) {
	gzr, err := gzip.NewReader(bytes.NewReader(gzData))
	if err != nil {
		return nil, err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	var out []*inMemoryFile
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch h.Typeflag {
		case tar.TypeReg:
			buf := &bytes.Buffer{}
			if _, err := io.Copy(buf, tr); err != nil {
				return nil, err
			}
			out = append(out, &inMemoryFile{
				Name: h.Name,
				Mode: h.Mode,
				Data: buf.Bytes(),
			})
		case tar.TypeDir:
			// 目录延迟创建
			out = append(out, &inMemoryFile{
				Name: h.Name + "/",
				Mode: h.Mode,
				Data: nil,
			})
		default:
			// 忽略其他类型
		}
	}
	return out, nil
}

func findFile(files []*inMemoryFile, name string) *inMemoryFile {
	for _, f := range files {
		if f.Name == name {
			return f
		}
	}
	return nil
}

func writeFiles(files []*inMemoryFile, base string) error {
	for _, f := range files {
		if strings.HasSuffix(f.Name, "/") {
			dir := filepath.Join(base, strings.TrimSuffix(f.Name, "/"))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			continue
		}
		dest := filepath.Join(base, f.Name)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o644
		}
		// Windows 下执行位不会实际影响 exe，可保留
		if err := os.WriteFile(dest, f.Data, mode); err != nil {
			return err
		}
	}
	return nil
}

func writeFilesWithLog(files []*inMemoryFile, base string) error {
	for i, f := range files {
		if strings.HasSuffix(f.Name, "/") {
			dir := filepath.Join(base, strings.TrimSuffix(f.Name, "/"))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			fmt.Printf("[%d/%d] 创建目录: %s\n", i+1, len(files), dir)
			continue
		}
		dest := filepath.Join(base, f.Name)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(dest, f.Data, mode); err != nil {
			return err
		}
		fmt.Printf("[%d/%d] 写入文件: %s (%d bytes)\n", i+1, len(files), dest, len(f.Data))
	}
	return nil
}

// ========== 自解压基础 ==========

func extractSelf() ([]byte, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(self)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// 1. 定义搜索范围：例如搜索末尾的 64KB
	// 如果文件被追加了签名，通常只有几 KB，64KB 足够覆盖
	const searchLimit = 64 * 1024
	fileSize := info.Size()
	if fileSize < trailerSize {
		return nil, fmt.Errorf("file too small")
	}

	readSize := int64(searchLimit)
	if readSize > fileSize {
		readSize = fileSize
	}

	// 2. 读取末尾数据块
	startOffset := fileSize - readSize
	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return nil, err
	}

	buf := make([]byte, readSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}

	// 3. 在缓冲区中倒序查找 Magic 字符串
	magicBytes := []byte(magicTrailer)
	idx := bytes.LastIndex(buf, magicBytes)
	if idx == -1 {
		return nil, fmt.Errorf("magic mismatch (signature not found in last %d bytes)", readSize)
	}

	// 4. 校验位置是否有足够的空间存放长度信息 (8 bytes)
	// Magic 在 buf[idx] 开始，长度信息应该在 buf[idx-8]
	if idx < 8 {
		// 这种情况极少见（Magic 刚好被切断在读取边界），但在 64KB 窗口下几乎不可能发生
		// 除非文件本身就极小且结构损坏
		return nil, fmt.Errorf("magic found but header truncated")
	}

	// 5. 解析长度
	lenStart := idx - 8
	archiveLen := binary.LittleEndian.Uint64(buf[lenStart : lenStart+8])

	// 6. 计算归档在文件中的绝对起始位置
	// buf[lenStart] 在文件中的位置是 startOffset + lenStart
	// 归档结束位置 = startOffset + lenStart
	// 归档开始位置 = 归档结束位置 - archiveLen
	archiveEndOffset := startOffset + int64(lenStart)
	archiveStartOffset := archiveEndOffset - int64(archiveLen)

	if archiveStartOffset < 0 {
		return nil, fmt.Errorf("invalid archive start offset")
	}

	// 7. 读取归档数据
	if _, err := f.Seek(archiveStartOffset, io.SeekStart); err != nil {
		return nil, err
	}

	archiveBuf := make([]byte, archiveLen)
	if _, err := io.ReadFull(f, archiveBuf); err != nil {
		return nil, err
	}

	return archiveBuf, nil
}

func decideInstallDir(productName, forced string) (string, error) {
	if forced != "" {
		return forced, os.MkdirAll(forced, 0o755)
	}
	if runtime.GOOS == "windows" {
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			path := filepath.Join(pf, productName)
			return path, os.MkdirAll(path, 0o755)
		}
	}
	cwd, _ := os.Getwd()
	path := filepath.Join(cwd, productName)
	return path, os.MkdirAll(path, 0o755)
}

// detectAnyExe: 若指定 exeName 不存在，兜底寻找一个 .exe
func detectAnyExe(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(strings.ToLower(name), ".exe") {
			return filepath.Join(root, name)
		}
	}
	// 进一步递归一层（可选）
	for _, e := range entries {
		if e.IsDir() {
			sub := filepath.Join(root, e.Name())
			files, _ := os.ReadDir(sub)
			for _, se := range files {
				if !se.IsDir() && strings.HasSuffix(strings.ToLower(se.Name()), ".exe") {
					return filepath.Join(sub, se.Name())
				}
			}
		}
	}
	return ""
}

// ========== 目录清理（安全） ==========

func cleanInstallDir(dir string) error {
	// 若不存在则直接创建由调用者继续
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("目标路径存在但不是目录: %s", dir)
	}
	// 安全保护：禁止删除过于顶层或敏感目录
	lower := strings.ToLower(filepath.Clean(dir))
	if lower == "c:/" || lower == "c:\\" || len(lower) <= 3 { // 例如 c:\ 或 d:\
		return fmt.Errorf("拒绝清理系统根目录: %s", dir)
	}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		lp := strings.ToLower(filepath.Clean(pf))
		if lower == lp { // 不能直接是 Program Files 根
			return fmt.Errorf("拒绝清理 ProgramFiles 根目录: %s", dir)
		}
	}
	// 额外保护：必须包含产品名（防止 meta 空 productName）
	if meta.ProductName == "" || !strings.Contains(lower, strings.ToLower(meta.ProductName)) {
		// 仅警告，不中断——但为了安全这里直接拒绝
		return fmt.Errorf("目录不包含产品名，取消清理: %s", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		// 不删除正在运行的卸载程序 / 安装程序（理论上不在此目录）
		full := filepath.Join(dir, name)
		if err := os.RemoveAll(full); err != nil {
			return fmt.Errorf("删除 %s 失败: %w", name, err)
		}
	}
	return nil
}
