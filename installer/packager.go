package installer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const magicTrailer = "SFXMAGIC"

type Options struct {
	ProductName             string
	ExeName                 string
	InstallDir              string
	CreateDesktopShortcut   bool
	CreateStartMenuShortcut bool
	Version                 string
	ShortcutName            string // 新增：快捷方式显示名称（为空则使用 ProductName）
	CompressionLevel        int    // 压缩等级，使用gzip包的常量（如gzip.BestCompression）
}

// CreateInstaller 将 payloadExe 打包并附加到 stubExe 生成 setup
func CreateInstaller(stubExe, payloadExe, outputSetup string, opts Options) error {
	payloadData, err := os.ReadFile(payloadExe)
	if err != nil {
		return fmt.Errorf("read payload: %w", err)
	}

	if opts.ExeName == "" {
		opts.ExeName = filepath.Base(payloadExe)
	}
	if opts.ProductName == "" {
		opts.ProductName = "MyApp"
	}

	if opts.ShortcutName == "" {
		opts.ShortcutName = opts.ProductName
	}

	meta := map[string]any{
		"productName":             opts.ProductName,
		"exeName":                 opts.ExeName,
		"installDir":              opts.InstallDir,
		"createDesktopShortcut":   opts.CreateDesktopShortcut,
		"createStartMenuShortcut": opts.CreateStartMenuShortcut,
		"version":                 opts.Version,
		"shortcutName":            opts.ShortcutName,
		"generatedAt":             time.Now().Format(time.RFC3339),
	}

	metaBytes, _ := json.MarshalIndent(meta, "", "  ")

	files := map[string][]byte{
		opts.ExeName: payloadData,
		"meta.json":  metaBytes,
	}

	// 设置默认压缩等级
	compressionLevel := gzip.NoCompression
	if opts.CompressionLevel == 9 {
		compressionLevel = gzip.BestCompression
	}
	
	archive, err := buildTarGz(files, compressionLevel)
	if err != nil {
		return fmt.Errorf("build archive: %w", err)
	}

	stubData, err := os.ReadFile(stubExe)
	if err != nil {
		return fmt.Errorf("read stub: %w", err)
	}

	f, err := os.OpenFile(outputSetup, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create setup: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(stubData); err != nil {
		return err
	}
	if _, err := f.Write(archive); err != nil {
		return err
	}

	lenBuf := make([]byte, 8)
	binary.LittleEndian.PutUint64(lenBuf, uint64(len(archive)))
	if _, err := f.Write(lenBuf); err != nil {
		return err
	}
	if _, err := f.Write([]byte(magicTrailer)); err != nil {
		return err
	}

	fmt.Printf("生成安装器: %s\n", outputSetup)
	fmt.Printf("  内含文件: %s, meta.json (%d bytes)\n", opts.ExeName, len(metaBytes))
	return nil
}

func buildTarGz(files map[string][]byte, compressionLevel int) ([]byte, error) {
	var buf bytes.Buffer
	gzw, err := gzip.NewWriterLevel(&buf, compressionLevel)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(gzw)

	now := time.Now()
	for name, data := range files {
		h := &tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    int64(len(data)),
			ModTime: now,
		}
		// 对于 exe 给予执行权限（在 *nix 上）
		if filepath.Ext(strings.ToLower(name)) == ".exe" {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			tw.Close()
			gzw.Close()
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			tw.Close()
			gzw.Close()
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		gzw.Close()
		return nil, err
	}
	if err := gzw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
