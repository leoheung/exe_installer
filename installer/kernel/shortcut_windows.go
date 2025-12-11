//go:build windows

package kernel

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

func CreateShortcuts(targetExe, workingDir string, meta InstallMeta) error {
	if _, err := os.Stat(targetExe); err != nil {
		return fmt.Errorf("target exe missing: %w", err)
	}

	if workingDir == "" {
		workingDir = filepath.Dir(targetExe)
	}

	iconPath := targetExe
	var errs []string

	name := meta.ShortcutName
	if name == "" {
		name = meta.ProductName
	}
	name = sanitizeFilename(name)

	if meta.CreateDesktopShortcut {
		fmt.Println(" - 正在创建桌面快捷方式...")
		if p, err := desktopDir(); err == nil {
			link := filepath.Join(p, name+".lnk")
			if err2 := createShortcut(link, targetExe, workingDir, iconPath); err2 != nil {
				errs = append(errs, "Desktop:"+err2.Error())
				fmt.Printf("   × 桌面快捷方式失败: %v\n", err2)
			} else {
				fmt.Printf("   √ 桌面快捷方式: %s\n", link)
			}
		} else {
			errs = append(errs, "DesktopDir:"+err.Error())
		}
	}

	if meta.CreateStartMenuShortcut {
		fmt.Println(" - 正在创建开始菜单快捷方式...")
		if p, err := startMenuDir(name); err == nil {
			if err = os.MkdirAll(p, 0o755); err != nil {
				errs = append(errs, "StartMenu mkdir:"+err.Error())
			} else {
				link := filepath.Join(p, name+".lnk")
				if err2 := createShortcut(link, targetExe, workingDir, iconPath); err2 != nil {
					errs = append(errs, "StartMenu:"+err2.Error())
					fmt.Printf("   × 开始菜单快捷方式失败: %v\n", err2)
				} else {
					fmt.Printf("   √ 开始菜单快捷方式: %s\n", link)
				}
			}
		} else {
			errs = append(errs, "StartMenuDir:"+err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf(strings.Join(errs, "; "))
	}
	return nil
}

func desktopDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Desktop"), nil
}

func startMenuDir(product string) (string, error) {
	appData := os.Getenv("AppData")
	if appData == "" {
		return "", fmt.Errorf("AppData env empty")
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", product), nil
}

func createShortcut(linkPath, targetPath, workingDir, iconPath string) error {
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return err
	}
	if workingDir == "" {
		workingDir = filepath.Dir(targetPath)
	}
	if iconPath == "" {
		iconPath = targetPath
	}

	// 优先使用底层 ShellLink 接口（完全 Unicode）
	if err := createShortcutShellLinkLowLevel(linkPath, targetPath, workingDir, iconPath); err == nil {
		return nil
	}

	// 回退：使用 WScript.Shell + IDispatch + 最终 VBScript 双层回退
	_ = ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED)
	defer ole.CoUninitialize()

	unknown, err := oleutil.CreateObject("WScript.Shell")
	if err != nil {
		return fallbackVbsShortcut(linkPath, targetPath, workingDir, iconPath, fmt.Errorf("CreateObject: %w", err))
	}
	defer unknown.Release()
	shell, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return fallbackVbsShortcut(linkPath, targetPath, workingDir, iconPath, fmt.Errorf("QI: %w", err))
	}
	defer shell.Release()

	shortcutDisp, err := oleutil.CallMethod(shell, "CreateShortcut", linkPath)
	if err != nil {
		return fallbackVbsShortcut(linkPath, targetPath, workingDir, iconPath, fmt.Errorf("CreateShortcut: %w", err))
	}
	shortcut := shortcutDisp.ToIDispatch()
	defer shortcut.Release()

	// 设置属性
	if _, err = oleutil.PutProperty(shortcut, "TargetPath", targetPath); err != nil {
		return fallbackVbsShortcut(linkPath, targetPath, workingDir, iconPath, fmt.Errorf("TargetPath: %w", err))
	}
	if _, err = oleutil.PutProperty(shortcut, "WorkingDirectory", workingDir); err != nil {
		return fallbackVbsShortcut(linkPath, targetPath, workingDir, iconPath, fmt.Errorf("WorkingDirectory: %w", err))
	}
	if _, err = oleutil.PutProperty(shortcut, "IconLocation", iconPath); err != nil {
		return fallbackVbsShortcut(linkPath, targetPath, workingDir, iconPath, fmt.Errorf("IconLocation: %w", err))
	}
	if _, err = oleutil.PutProperty(shortcut, "WindowStyle", 1); err != nil {
		return fallbackVbsShortcut(linkPath, targetPath, workingDir, iconPath, fmt.Errorf("WindowStyle: %w", err))
	}

	if _, err = oleutil.CallMethod(shortcut, "Save"); err != nil {
		return fallbackVbsShortcut(linkPath, targetPath, workingDir, iconPath, fmt.Errorf("Save: %w", err))
	}
	// 验证文件是否真的创建（某些奇怪的 locale 下 Save 返回成功但文件不存在）
	if _, statErr := os.Stat(linkPath); statErr != nil {
		return fallbackVbsShortcut(linkPath, targetPath, workingDir, iconPath, fmt.Errorf("post-save missing: %w", statErr))
	}
	return nil
}

var invalidFileChars = regexp.MustCompile(`[\\/:*?"<>|]`)

func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "_"
	}
	s = invalidFileChars.ReplaceAllString(s, "_")
	// Windows 不允许 结尾点或空格
	s = strings.TrimRight(s, ". ")
	if s == "" {
		return "_"
	}
	return s
}

// ---------------- Low-level Shell Link (IShellLinkW + IPersistFile) -----------------

// GUID 定义
var (
	CLSID_ShellLink  = ole.GUID{Data1: 0x00021401, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	IID_IShellLinkW  = ole.GUID{Data1: 0x000214F9, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	IID_IPersistFile = ole.GUID{Data1: 0x0000010b, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
)

const (
	clsctxInprocServer = 0x1
	SW_SHOWNORMAL      = 1
)

type IShellLinkW struct{ lpVtbl *IShellLinkWVtbl }

type IShellLinkWVtbl struct {
	QueryInterface      uintptr
	AddRef              uintptr
	Release             uintptr
	GetPath             uintptr
	GetIDList           uintptr
	SetIDList           uintptr
	GetDescription      uintptr
	SetDescription      uintptr
	GetWorkingDirectory uintptr
	SetWorkingDirectory uintptr
	GetArguments        uintptr
	SetArguments        uintptr
	GetHotkey           uintptr
	SetHotkey           uintptr
	GetShowCmd          uintptr
	SetShowCmd          uintptr
	GetIconLocation     uintptr
	SetIconLocation     uintptr
	SetRelativePath     uintptr
	Resolve             uintptr
	SetPath             uintptr
}

type IPersistFile struct{ lpVtbl *IPersistFileVtbl }

type IPersistFileVtbl struct {
	// IUnknown
	QueryInterface uintptr
	AddRef         uintptr
	Release        uintptr
	// IPersist
	GetClassID uintptr
	// IPersistFile
	IsDirty       uintptr
	Load          uintptr
	Save          uintptr
	SaveCompleted uintptr
	GetCurFile    uintptr
}

func createShortcutShellLinkLowLevel(linkPath, targetPath, workingDir, iconPath string) error {
	// 初始化 COM (允许外部已初始化)
	_ = ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED)
	// 不使用 defer CoUninitialize() 以免与上层重复释放；由上层统一处理

	var ppv unsafe.Pointer
	hr, _, _ := syscall.Syscall6(procCoCreateInstance.Addr(), 5,
		uintptr(unsafe.Pointer(&CLSID_ShellLink)), 0, uintptr(clsctxInprocServer),
		uintptr(unsafe.Pointer(&IID_IShellLinkW)), uintptr(unsafe.Pointer(&ppv)), 0)
	if failed(hr) {
		return fmt.Errorf("CoCreateInstance shelllink hr=0x%x", hr)
	}
	shellLink := (*IShellLinkW)(ppv)
	defer comRelease(unsafe.Pointer(shellLink))

	if workingDir == "" {
		workingDir = filepath.Dir(targetPath)
	}
	if iconPath == "" {
		iconPath = targetPath
	}

	if err := slSetPath(shellLink, targetPath); err != nil {
		return err
	}
	if err := slSetWorkingDir(shellLink, workingDir); err != nil {
		return err
	}
	if err := slSetShowCmd(shellLink, SW_SHOWNORMAL); err != nil {
		return err
	}
	if err := slSetIcon(shellLink, iconPath, 0); err != nil {
		// 非致命，继续
	}

	// IPersistFile 保存
	var ppvFile unsafe.Pointer
	hr, _, _ = syscall.Syscall(shellLink.lpVtbl.QueryInterface, 3,
		uintptr(unsafe.Pointer(shellLink)), uintptr(unsafe.Pointer(&IID_IPersistFile)), uintptr(unsafe.Pointer(&ppvFile)))
	if failed(hr) {
		return fmt.Errorf("QueryInterface IPersistFile hr=0x%x", hr)
	}
	persist := (*IPersistFile)(ppvFile)
	defer comRelease(unsafe.Pointer(persist))

	wlink, _ := syscall.UTF16PtrFromString(linkPath)
	// Save(lpFileName, TRUE)
	hr, _, _ = syscall.Syscall(persist.lpVtbl.Save, 3,
		uintptr(unsafe.Pointer(persist)), uintptr(unsafe.Pointer(wlink)), 1)
	if failed(hr) {
		return fmt.Errorf("PersistFile.Save hr=0x%x", hr)
	}
	// 验证存在
	if _, err := os.Stat(linkPath); err != nil {
		return fmt.Errorf("saved-missing: %v", err)
	}
	return nil
}

// Windows API / helper 部分
var (
	ole32                = syscall.NewLazyDLL("ole32.dll")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procIUnknownRelease  = ole32.NewProc("_IUnknown_Release@4") // 可能不可用，改用 vtable
)

func failed(hr uintptr) bool { return int32(hr) < 0 }

func slSetPath(sl *IShellLinkW, path string) error {
	w, _ := syscall.UTF16PtrFromString(path)
	hr, _, _ := syscall.Syscall(sl.lpVtbl.SetPath, 2, uintptr(unsafe.Pointer(sl)), uintptr(unsafe.Pointer(w)), 0)
	if failed(hr) {
		return fmt.Errorf("SetPath hr=0x%x", hr)
	}
	return nil
}
func slSetWorkingDir(sl *IShellLinkW, dir string) error {
	w, _ := syscall.UTF16PtrFromString(dir)
	hr, _, _ := syscall.Syscall(sl.lpVtbl.SetWorkingDirectory, 2, uintptr(unsafe.Pointer(sl)), uintptr(unsafe.Pointer(w)), 0)
	if failed(hr) {
		return fmt.Errorf("SetWorkingDirectory hr=0x%x", hr)
	}
	return nil
}
func slSetShowCmd(sl *IShellLinkW, cmd int) error {
	hr, _, _ := syscall.Syscall(sl.lpVtbl.SetShowCmd, 2, uintptr(unsafe.Pointer(sl)), uintptr(cmd), 0)
	if failed(hr) {
		return fmt.Errorf("SetShowCmd hr=0x%x", hr)
	}
	return nil
}
func slSetIcon(sl *IShellLinkW, icon string, index int) error {
	w, _ := syscall.UTF16PtrFromString(icon)
	hr, _, _ := syscall.Syscall(sl.lpVtbl.SetIconLocation, 3, uintptr(unsafe.Pointer(sl)), uintptr(unsafe.Pointer(w)), uintptr(int32(index)))
	if failed(hr) {
		return fmt.Errorf("SetIconLocation hr=0x%x", hr)
	}
	return nil
}

// comRelease 调用对象 vtable 中的 Release (第 3 个槽位)
func comRelease(p unsafe.Pointer) {
	if p == nil {
		return
	}
	// 取出 vtable 地址
	vtbl := *(*uintptr)(p)
	// 构造一个切片头读取前三个函数指针
	var slice []uintptr
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&slice))
	hdr.Data = vtbl
	hdr.Len = 3
	hdr.Cap = 3
	releasePtr := slice[2]
	syscall.Syscall(releasePtr, 1, uintptr(p), 0, 0)
}

// fallbackVbsShortcut 尝试使用临时 VBScript 创建快捷方式 (UTF-16 LE BOM) 以提升兼容性
func fallbackVbsShortcut(linkPath, targetPath, workingDir, iconPath string, originalErr error) error {
	// 若已经存在则不重复
	if _, err := os.Stat(linkPath); err == nil {
		return nil
	}
	// 生成 VBScript
	script := fmt.Sprintf(`Option Explicit
Dim shell, lnk
Set shell = CreateObject("WScript.Shell")
Set lnk = shell.CreateShortcut(%q)
lnk.TargetPath = %q
lnk.WorkingDirectory = %q
lnk.IconLocation = %q
lnk.WindowStyle = 1
lnk.Save
`, linkPath, targetPath, workingDir, iconPath)

	tmpDir := os.TempDir()
	name := fmt.Sprintf("shortcut_%d.vbs", time.Now().UnixNano())
	vbsPath := filepath.Join(tmpDir, name)

	// 写入 UTF-16 LE BOM
	utf16le, err := toUTF16LEWithBOM(script)
	if err != nil {
		return fmt.Errorf("fallback encode: %w; original: %v", err, originalErr)
	}
	if err = os.WriteFile(vbsPath, utf16le, 0o600); err != nil {
		return fmt.Errorf("fallback write: %w; original: %v", err, originalErr)
	}
	defer os.Remove(vbsPath)

	cmd := exec.Command("cscript.exe", "//NoLogo", vbsPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("fallback vbs failed: %v output=%s original=%v", err, strings.TrimSpace(string(out)), originalErr)
	}
	if _, err2 := os.Stat(linkPath); err2 != nil {
		return fmt.Errorf("fallback vbs no link: %v original=%v", err2, originalErr)
	}
	return nil
}

func toUTF16LEWithBOM(s string) ([]byte, error) {
	// Simple manual UTF-16LE encoding
	// (不使用 golang.org/x/text/encoding 避免额外依赖)
	var buf []byte
	buf = append(buf, 0xFF, 0xFE) // BOM
	for _, r := range s {
		if r > 0xFFFF { // surrogate pair
			// Encode as UTF-16 surrogate pair
			r1, r2 := utf16EncodeRune(r)
			buf = append(buf, byte(r1), byte(r1>>8), byte(r2), byte(r2>>8))
		} else {
			buf = append(buf, byte(r), byte(r>>8))
		}
	}
	return buf, nil
}

func utf16EncodeRune(r rune) (uint16, uint16) {
	if r <= 0xFFFF {
		return uint16(r), 0
	}
	r -= 0x10000
	return uint16(0xD800 + (r >> 10)), uint16(0xDC00 + (r & 0x3FF))
}

// 为了简化，检查第二个 uint16 是否 0 决定是否附加
func appendUTF16Rune(dst []byte, r rune) []byte { // unused helper (留作扩展)
	a, b := utf16EncodeRune(r)
	dst = append(dst, byte(a), byte(a>>8))
	if b != 0 {
		dst = append(dst, byte(b), byte(b>>8))
	}
	return dst
}

var _ = errors.New // keep errors import if not otherwise used
