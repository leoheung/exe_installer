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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
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
	DownloadURL             string `json:"downloadURL"`
	NumWorkers              int    `json:"numWorkers"`
}

// 下载功能相关代码（从download.go复制）
// DownloadFileByConcurrent 多Goroutine分片下载文件（自动保留原始后缀）
// 参数说明：
//
// url: 下载URL（你的GetApp接口地址，string）
// outputPath: 保存目录/文件路径（string）：
//   - 若为目录（如./downloads/），自动拼接原始文件名；
//   - 若为文件路径（如./app.exe），直接使用该路径；
//
// num_workers: 并发数（必须>0，int）
func DownloadFileByConcurrent(url string, outputPath string, num_workers int /* remove chunkSize */) error {
	// 1. 参数合法性校验
	if url == "" {
		return errors.New("url cannot be empty")
	}
	if outputPath == "" {
		return errors.New("outputPath cannot be empty")
	}
	if num_workers <= 0 {
		return fmt.Errorf("invalid num_workers: %d (must > 0)", num_workers)
	}

	// 2. 获取原始文件名（含后缀）
	originalFilename, err := getOriginalFilename(url)
	if err != nil {
		return fmt.Errorf("get original filename failed: %w", err)
	}

	// 3. 处理输出路径：如果是目录，拼接原始文件名
	targetPath, err := resolveOutputPath(outputPath, originalFilename)
	if err != nil {
		return fmt.Errorf("resolve output path failed: %w", err)
	}

	// 4. 先请求获取文件总大小（HEAD请求）
	fileSize, err := getFileSize(url)
	if err != nil {
		return fmt.Errorf("get file size failed: %w", err)
	}
	if fileSize <= 0 {
		return errors.New("invalid file size")
	}

	// 5. 计算分片大小与分片切分：
	// 自动计算 chunkSize = ceil(fileSize / num_workers)
	// 若 num_workers 大于文件大小字节数，至少保证 chunkSize=1
	chunkSize := fileSize / int64(num_workers)
	if fileSize%int64(num_workers) != 0 {
		chunkSize++
	}
	if chunkSize <= 0 {
		chunkSize = 1
	}

	// 5. 计算分片数和每个分片的起止字节
	chunks := calculateChunks(fileSize, chunkSize)
	if len(chunks) == 0 {
		return errors.New("no chunks to download")
	}
	// 调整并发数：如果分片数少于指定的并发数，用分片数作为实际并发数
	actualWorkers := num_workers
	if actualWorkers > len(chunks) {
		actualWorkers = len(chunks)
		fmt.Printf("adjust num_workers to %d (match chunk count)\n", actualWorkers)
	}

	// 6. 初始化临时文件（存储各分片，最后合并）
	tempFiles := make([]*os.File, len(chunks))
	defer func() {
		// 最后关闭并删除临时文件
		for _, f := range tempFiles {
			if f != nil {
				_ = f.Close()
				_ = os.Remove(f.Name())
			}
		}
	}()
	for i := range chunks {
		tempFile, err := os.CreateTemp("", fmt.Sprintf("chunk-%d-*", i))
		if err != nil {
			return fmt.Errorf("create temp file %d failed: %w", i, err)
		}
		tempFiles[i] = tempFile
	}

	// 7. 启动Goroutine下载分片
	var wg sync.WaitGroup
	var downloadErr atomic.Value // 存储下载错误（原子操作保证并发安全）
	var downloadedSize int64     // 已下载字节数（原子更新）

	sem := make(chan struct{}, actualWorkers) // 并发控制信号量
	// 修正：直接使用 i 和 chunks[i]，避免声明未使用的 chunk 变量
	for i := range chunks {
		sem <- struct{}{} // 占用信号量
		wg.Add(1)
		go func(idx int, c chunkInfo) {
			defer func() {
				<-sem // 释放信号量
				wg.Done()
			}()

			// 如果已有错误，直接返回
			if err := downloadErr.Load(); err != nil {
				return
			}

			// 下载当前分片（带重试）
			err := downloadChunk(url, tempFiles[idx], c.start, c.end, &downloadedSize)
			if err != nil {
				downloadErr.Store(fmt.Errorf("download chunk %d failed: %w", idx, err))
				return
			}
		}(i, chunks[i])
	}

	// 实时更新GUI下载进度
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			cur := atomic.LoadInt64(&downloadedSize)
			progress := int(float64(cur) / float64(fileSize) * 100)
			fmt.Printf("\rDownload progress: %.2f%% (%.2fMB/%.2fMB)",
				float64(progress),
				float64(cur)/1024/1024,
				float64(fileSize)/1024/1024)
			// 更新GUI进度条
			updateProgress(progress)
			if progress >= 100 {
				fmt.Println()
				return
			}
			// early exit if a download error occurred
			if errVal := downloadErr.Load(); errVal != nil {
				fmt.Println()
				return
			}
		}
	}()

	// 8. 等待所有分片下载完成
	wg.Wait()
	// 检查是否有下载错误
	if errVal := downloadErr.Load(); errVal != nil {
		return errVal.(error)
	}
	fmt.Println("\nAll chunks downloaded, merging...")

	// 9. 合并分片到最终文件
	if err := mergeChunks(tempFiles, targetPath); err != nil {
		return fmt.Errorf("merge chunks failed: %w", err)
	}

	fmt.Printf("Download success! File saved to: %s\n", targetPath)
	return nil
}

// ------------------------- 新增：解析原始文件名和输出路径 -------------------------
// getOriginalFilename 从URL/响应头中提取原始文件名（含后缀）
func getOriginalFilename(urlStr string) (string, error) {
	// 第一步：发送HEAD请求，获取响应头中的文件名
	req, err := http.NewRequest("HEAD", urlStr, nil)
	if err != nil {
		return "", err
	}
	// 携带GitHub Token（如果你的服务端需要）
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "token "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 从Content-Disposition头提取文件名（优先级最高）
	if disp := resp.Header.Get("Content-Disposition"); disp != "" {
		// 匹配格式：attachment; filename="app.exe" 或 filename=app.exe
		parts := strings.Split(disp, ";")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "filename=") {
				filename := strings.TrimPrefix(part, "filename=")
				filename = strings.Trim(filename, `"'`) // 去掉引号
				if filename != "" {
					return filename, nil
				}
			}
		}
	}

	// 第二步：从URL路径中解析文件名
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return "", err
	}
	filename := filepath.Base(parsedURL.Path)
	if filename != "" && filename != "/" && filename != "." {
		return filename, nil
	}

	// 兜底：生成默认文件名
	return "downloaded_file", nil
}

// resolveOutputPath 处理输出路径：如果是目录则拼接文件名，否则直接使用
func resolveOutputPath(outputPath, filename string) (string, error) {
	// 检查outputPath是否是目录
	fileInfo, err := os.Stat(outputPath)
	if err == nil && fileInfo.IsDir() {
		// 是目录：拼接目录+文件名
		return filepath.Join(outputPath, filename), nil
	}

	// 不是目录（或不存在）：检查父目录是否存在，不存在则创建
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return outputPath, nil
}

// ------------------------- 原有辅助函数（无修改） -------------------------
type chunkInfo struct {
	start int64 // 分片起始字节
	end   int64 // 分片结束字节
}

// getFileSize 获取文件总大小（HEAD请求）
func getFileSize(url string) (int64, error) {
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("head request failed: %s", resp.Status)
	}

	sizeStr := resp.Header.Get("Content-Length")
	if sizeStr == "" {
		return 0, errors.New("content-length header not found")
	}
	return strconv.ParseInt(sizeStr, 10, 64)
}

// calculateChunks 计算分片的起止字节
func calculateChunks(fileSize, chunkSize int64) []chunkInfo {
	var chunks []chunkInfo
	for start := int64(0); start < fileSize; start += chunkSize {
		end := start + chunkSize - 1
		if end >= fileSize {
			end = fileSize - 1
		}
		chunks = append(chunks, chunkInfo{start: start, end: end})
	}
	return chunks
}

// downloadChunk 下载单个分片（带重试）
func downloadChunk(url string, file *os.File, start, end int64, downloadedSize *int64) error {
	// retry up to 3 times
	for retry := 0; retry < 3; retry++ {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			if retry == 2 {
				return err
			}
			continue
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if retry == 2 {
				return err
			}
			continue
		}

		// ensure body is closed immediately after use (no defer in loop)
		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			if retry == 2 {
				return fmt.Errorf("invalid status code: %d", resp.StatusCode)
			}
			continue
		}

		n, err := io.Copy(file, resp.Body)
		resp.Body.Close()
		if err != nil {
			if retry == 2 {
				return err
			}
			continue
		}
		atomic.AddInt64(downloadedSize, n)
		return nil
	}
	return errors.New("retry 3 times still failed")
}

// mergeChunks 合并所有分片到最终文件
func mergeChunks(tempFiles []*os.File, outputPath string) error {
	// 创建最终文件
	outFile, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	// 按顺序合并每个分片
	for _, f := range tempFiles {
		// 回到文件开头
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		// 拷贝分片数据到最终文件
		if _, err := io.Copy(outFile, f); err != nil {
			return err
		}
	}
	return nil
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

// GUI相关变量
var (
	mainWindow *walk.MainWindow

	// 安装页面变量
	welcomeLabel   *walk.Label
	statusLabel    *walk.Label
	installDirEdit *walk.LineEdit
	installGroup   *walk.GroupBox
	installButtons *walk.Composite

	// 进度页面变量
	progressLabel *walk.Label
	progressBar   *walk.ProgressBar
	progressGroup *walk.GroupBox

	// 结果页面变量
	resultLabel   *walk.Label
	runAppButton  *walk.PushButton
	exitAppButton *walk.PushButton
	resultGroup   *walk.Composite

	archiveData    []byte
	filesToInstall []*inMemoryFile
	installMutex   sync.Mutex
	installStatus  string

	// 安装结果
	installedExePath string
	installedDir     string
)

func main() {
	if isUninstallMode() {
		runUninstall()
		return
	}

	// 启动GUI
	startGUI()
}

// startGUI 启动图形用户界面
func startGUI() {
	var err error

	// 创建安装目录选择按钮
	var browseBtn *walk.PushButton
	var installBtn *walk.PushButton
	var exitBtn *walk.PushButton

	// 主窗口布局
	MainWindow{
		AssignTo: &mainWindow,
		Title:    fmt.Sprintf("%s 安装程序", meta.ProductName),
		Size:     Size{Width: 500, Height: 300},
		Layout:   VBox{},
		Children: []Widget{
			// 安装页面控件
			Label{
				AssignTo: &welcomeLabel,
				Text:     fmt.Sprintf("欢迎使用 %s 安装程序", meta.ProductName),
				Font:     Font{PointSize: 14, Bold: true},
			},
			Label{
				AssignTo: &statusLabel,
				Text:     "正在准备安装...",
			},
			GroupBox{
				AssignTo:      &installGroup,
				Title:         "安装选项",
				Layout:        Grid{Columns: 1},
				StretchFactor: 1,
				Children: []Widget{
					Composite{
						Layout: HBox{},
						Children: []Widget{
							Label{
								Text: "安装目录:",
							},
							LineEdit{
								AssignTo:      &installDirEdit,
								Text:          "",
								StretchFactor: 1,
							},
							PushButton{
								AssignTo: &browseBtn,
								Text:     "浏览...",
								OnClicked: func() {
									browseForInstallDir()
								},
							},
						},
					},
					CheckBox{
						Text:    "创建桌面快捷方式",
						Checked: meta.CreateDesktopShortcut,
						OnClicked: func() {
							meta.CreateDesktopShortcut = !meta.CreateDesktopShortcut
						},
					},
					CheckBox{
						Text:    "创建开始菜单快捷方式",
						Checked: meta.CreateStartMenuShortcut,
						OnClicked: func() {
							meta.CreateStartMenuShortcut = !meta.CreateStartMenuShortcut
						},
					},
				},
			},
			// 进度页面控件
			GroupBox{
				AssignTo:      &progressGroup,
				Title:         "安装进度",
				Layout:        VBox{},
				StretchFactor: 1,
				Visible:       false,
				Children: []Widget{
					Label{
						AssignTo: &progressLabel,
						Text:     "正在安装...",
						Font:     Font{PointSize: 12, Bold: true},
					},
					ProgressBar{
						AssignTo:      &progressBar,
						Value:         0,
						MaxValue:      100,
						StretchFactor: 1,
					},
				},
			},
			// 结果页面控件
			Composite{
				AssignTo:      &resultGroup,
				Layout:        VBox{},
				StretchFactor: 1,
				Visible:       false,
				Children: []Widget{
					Label{
						Text: "安装完成",
						Font: Font{PointSize: 18, Bold: true},
					},
					Label{
						AssignTo: &resultLabel,
						Text:     "",
					},
					Composite{
						Layout: HBox{},
						Children: []Widget{
							PushButton{
								AssignTo: &runAppButton,
								Text:     "立即运行",
								OnClicked: func() {
									runInstalledApplication()
								},
								StretchFactor: 1,
							},
							PushButton{
								AssignTo: &exitAppButton,
								Text:     "退出",
								OnClicked: func() {
									mainWindow.Close()
								},
								StretchFactor: 1,
							},
						},
					},
				},
			},
			// 底部按钮栏
			Composite{
				AssignTo: &installButtons,
				Layout: HBox{
					MarginsZero: true,
					SpacingZero: true,
				},
				Children: []Widget{
					PushButton{
						AssignTo: &installBtn,
						Text:     "安装",
						OnClicked: func() {
							startInstall()
						},
						StretchFactor: 1,
					},
					PushButton{
						AssignTo: &exitBtn,
						Text:     "退出",
						OnClicked: func() {
							mainWindow.Close()
						},
						StretchFactor: 1,
					},
				},
			},
		},
	}.Create()

	if err != nil {
		fmt.Printf("窗口创建失败: %v\n", err)
		return
	}

	// 提取自解压数据
	go func() {
		var err error
		archiveData, err = extractSelf()
		if err != nil {
			updateStatus(fmt.Sprintf("无法提取内置归档: %v", err))
			return
		}

		updateStatus("正在解压归档...")
		filesToInstall, err = untarGzToMemory(archiveData)
		if err != nil {
			updateStatus(fmt.Sprintf("解包归档失败: %v", err))
			return
		}

		// 解析 meta.json
		if m := findFile(filesToInstall, "meta.json"); m != nil {
			_ = json.Unmarshal(m.Data, &meta) // 宽松处理
		}

		// 更新窗口标题和欢迎信息
		mainWindow.Synchronize(func() {
			mainWindow.SetTitle(fmt.Sprintf("%s 安装程序", meta.ProductName))
			if welcomeLabel != nil {
				welcomeLabel.SetText(fmt.Sprintf("欢迎使用 %s 安装程序", meta.ProductName))
			}
		})

		// 设置默认安装目录
		defaultInstallDir, _ := decideInstallDir(meta.ProductName, meta.InstallDir)
		installMutex.Lock()
		if installDirEdit != nil {
			installDirEdit.SetText(defaultInstallDir)
		}
		installMutex.Unlock()

		updateStatus(fmt.Sprintf("版本: %s", meta.Version))
	}()

	// 显示主窗口
	mainWindow.Run()
}

// browseForInstallDir 打开目录选择对话框
func browseForInstallDir() {
	defaultDir := installDirEdit.Text()
	if defaultDir == "" {
		defaultDir, _ = decideInstallDir(meta.ProductName, meta.InstallDir)
	}

	// 创建文件夹对话框
	fd := new(walk.FileDialog)
	fd.Title = "选择安装目录"
	fd.FilePath = defaultDir

	if ok, err := fd.ShowBrowseFolder(mainWindow); err != nil {
		walk.MsgBox(mainWindow, "错误", fmt.Sprintf("目录选择对话框错误: %v", err), walk.MsgBoxIconError)
		return
	} else if ok {
		installDirEdit.SetText(fd.FilePath)
	}
}

// startInstall 开始安装过程
func startInstall() {
	installDir := installDirEdit.Text()
	if installDir == "" {
		walk.MsgBox(mainWindow, "错误", "请选择安装目录", walk.MsgBoxIconError)
		return
	}

	// 切换到进度页面
	mainWindow.Synchronize(func() {
		welcomeLabel.SetVisible(false)
		statusLabel.SetVisible(false)
		installGroup.SetVisible(false)
		installButtons.SetVisible(false)
		progressGroup.SetVisible(true)
		resultGroup.SetVisible(false)
	})

	// 重置进度条
	updateProgress(0)

	// 启动安装线程
	go func() {
		var exePath string
		var err error
		var downloadedFile string

		defer func() {
			if err != nil {
				// 如果发生错误，切换回安装页面
				mainWindow.Synchronize(func() {
					welcomeLabel.SetVisible(true)
					statusLabel.SetVisible(true)
					installGroup.SetVisible(true)
					installButtons.SetVisible(true)
					progressGroup.SetVisible(false)
					resultGroup.SetVisible(false)
				})
				walk.MsgBox(mainWindow, "错误", fmt.Sprintf("安装失败: %v", err), walk.MsgBoxIconError)
				return
			}

			// 安装完成，切换到结果页面
			mainWindow.Synchronize(func() {
				progressGroup.SetVisible(false)
				resultGroup.SetVisible(true)
				installButtons.SetVisible(false)
			})

			// 保存安装结果
			installedDir = installDir
			installedExePath = exePath

			// 更新结果页面信息
			if resultLabel != nil {
				resultLabel.SetText(fmt.Sprintf("%s 已成功安装到 %s", meta.ProductName, installDir))
			}
		}()

		updateStatus("正在清理安装目录...")
		if err := cleanInstallDir(installDir); err != nil {
			return
		}

		// 如果有下载URL，则先下载文件
		if meta.DownloadURL != "" {
			updateStatus("正在下载文件...")

			// 设置下载的目标路径
			downloadedFile = filepath.Join(installDir, meta.ExeName)

			// 设置默认并发数
			numWorkers := 4
			if meta.NumWorkers > 0 {
				numWorkers = meta.NumWorkers
			}

			// 调用下载函数
			if err := DownloadFileByConcurrent(meta.DownloadURL, downloadedFile, numWorkers); err != nil {
				updateStatus(fmt.Sprintf("下载失败: %v", err))
				return
			}

			updateStatus("下载完成")
		} else {
			// 离线安装模式
			if filesToInstall == nil {
				err = errors.New("安装文件尚未加载完成，请稍候")
				return
			}

			updateStatus("正在写入文件...")
			if err := writeFilesWithProgress(filesToInstall, installDir); err != nil {
				return
			}
		}

		// 确定实际 exe 路径
		exePath = filepath.Join(installDir, meta.ExeName)
		if _, err := os.Stat(exePath); err != nil {
			updateStatus(fmt.Sprintf("未找到指定主程序 %s，尝试自动查找...", meta.ExeName))
			if detected := detectAnyExe(installDir); detected != "" {
				updateStatus(fmt.Sprintf("自动发现可执行文件: %s", detected))
				exePath = detected
			} else {
				updateStatus("未发现任何 .exe，跳过快捷方式创建")
			}
		}

		if runtime.GOOS == "windows" && (meta.CreateDesktopShortcut || meta.CreateStartMenuShortcut) {
			updateStatus("正在创建快捷方式...")
			if err := createShortcuts(exePath, installDir, meta); err != nil {
				updateStatus(fmt.Sprintf("创建快捷方式失败（忽略）：%v", err))
			} else {
				updateStatus("快捷方式创建完成")
			}
		}

		// 生成卸载程序并写入注册表（仅 Windows 生效）
		if runtime.GOOS == "windows" {
			updateStatus("正在创建卸载程序...")
			if err := createUninstaller(installDir); err != nil {
				updateStatus(fmt.Sprintf("创建卸载程序失败（忽略）：%v", err))
			}
			if err := writeRegistry(meta, installDir, exePath); err != nil {
				updateStatus(fmt.Sprintf("写入注册表失败（忽略）：%v", err))
			} else {
				updateStatus("已写入注册表信息")
			}
		}

		updateStatus("安装完成")
		updateProgress(100)
	}()
}

// openInstallDirectory 打开安装目录
func openInstallDirectory() {
	if installedDir == "" {
		return
	}

	// 在不同平台上打开目录
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "windows":
		cmd = "explorer"
		args = []string{installedDir}
	case "darwin": // macOS
		cmd = "open"
		args = []string{installedDir}
	default: // Linux
		cmd = "xdg-open"
		args = []string{installedDir}
	}

	_, err := os.StartProcess(cmd, args, &os.ProcAttr{
		Env:   os.Environ(),
		Files: []*os.File{nil, nil, nil},
	})
	if err != nil {
		walk.MsgBox(mainWindow, "错误", fmt.Sprintf("无法打开目录: %v", err), walk.MsgBoxIconError)
	}
}

// runInstalledApplication 运行已安装的应用程序
func runInstalledApplication() {
	if installedExePath == "" {
		return
	}

	// 运行安装的程序
	_, err := os.StartProcess(installedExePath, []string{}, &os.ProcAttr{
		Dir:   installedDir,
		Env:   os.Environ(),
		Files: []*os.File{nil, nil, nil},
	})
	if err != nil {
		walk.MsgBox(mainWindow, "错误", fmt.Sprintf("无法运行程序: %v", err), walk.MsgBoxIconError)
		return
	}

	// 运行成功后关闭安装程序
	mainWindow.Close()
}

// updateStatus 更新状态标签文本
func updateStatus(status string) {
	installMutex.Lock()
	installStatus = status
	installMutex.Unlock()

	// 在GUI线程中更新标签
	if mainWindow != nil {
		mainWindow.Synchronize(func() {
			if statusLabel != nil {
				statusLabel.SetText(status)
			}
			// 同时更新进度页面的标签
			if progressLabel != nil {
				progressLabel.SetText(status)
			}
		})
	}
}

// updateProgress 更新进度条
func updateProgress(value int) {
	if mainWindow != nil {
		mainWindow.Synchronize(func() {
			if progressBar != nil {
				progressBar.SetValue(value)
			}
		})
	}
}

// writeFilesWithProgress 带进度的文件写入
func writeFilesWithProgress(files []*inMemoryFile, base string) error {
	for i, f := range files {
		// 更新进度
		progress := int(float64(i+1) / float64(len(files)) * 100)
		updateStatus(fmt.Sprintf("正在写入文件 (%d/%d) - %s", i+1, len(files), f.Name))
		updateProgress(progress)

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

		if err := os.WriteFile(dest, f.Data, mode); err != nil {
			return err
		}

		// 短暂延迟以确保GUI有机会更新
		time.Sleep(10 * time.Millisecond)
	}

	return nil
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

// pressAnyKey 等待用户按键（仅在命令行模式下使用）
func pressAnyKey() error {
	fmt.Print("按回车退出...")
	_, err := fmt.Scanln()
	return err
}
