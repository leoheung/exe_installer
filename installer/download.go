package installer

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DownloadFileByConcurrent 多Goroutine分片下载文件（自动保留原始后缀）
// 参数说明：
//
//	url: 下载URL（你的GetApp接口地址，string）
//	outputPath: 保存目录/文件路径（string）：
//	  - 若为目录（如./downloads/），自动拼接原始文件名；
//	  - 若为文件路径（如./app.exe），直接使用该路径；
//	num_workers: 并发数（必须>0，int）
func DownloadFileByConcurrent(url string, outputPath string, num_workers int /* remove chunkSize */) (string,error) {
	// 1. 参数合法性校验
	if url == "" {
		return "",errors.New("url cannot be empty")
	}
	if outputPath == "" {
		return "",errors.New("outputPath cannot be empty")
	}
	if num_workers <= 0 {
		return "",fmt.Errorf("invalid num_workers: %d (must > 0)", num_workers)
	}

	// 2. 获取原始文件名（含后缀）
	originalFilename, err := getOriginalFilename(url)
	if err != nil {
		return "",fmt.Errorf("get original filename failed: %w", err)
	}

	// 3. 处理输出路径：如果是目录，拼接原始文件名
	targetPath, err := resolveOutputPath(outputPath, originalFilename)
	if err != nil {
		return "",fmt.Errorf("resolve output path failed: %w", err)
	}

	// 4. 先请求获取文件总大小（HEAD请求）
	fileSize, err := getFileSize(url)
	if err != nil {
		return "",fmt.Errorf("get file size failed: %w", err)
	}
	if fileSize <= 0 {
		return "",errors.New("invalid file size")
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
		return "",errors.New("no chunks to download")
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
			return "",fmt.Errorf("create temp file %d failed: %w", i, err)
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

	// 实时打印下载进度
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			cur := atomic.LoadInt64(&downloadedSize)
			progress := float64(cur) / float64(fileSize) * 100
			fmt.Printf("\rDownload progress: %.2f%% (%.2fMB/%.2fMB)",
				progress,
				float64(cur)/1024/1024,
				float64(fileSize)/1024/1024)
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
		return "",errVal.(error)
	}
	fmt.Println("\nAll chunks downloaded, merging...")

	// 9. 合并分片到最终文件
	if err := mergeChunks(tempFiles, targetPath); err != nil {
		return "",fmt.Errorf("merge chunks failed: %w", err)
	}

	fmt.Printf("Download success! File saved to: %s\n", targetPath)
	return originalFilename,nil
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
				filename = strings.Trim(filename, "\"'") // 去掉引号
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


