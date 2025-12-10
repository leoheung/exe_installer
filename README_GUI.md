# GUI 安装程序使用说明

本项目提供了一个带图形用户界面(GUI)的自解压安装程序生成器。

## 主要功能

- ✅ 图形化安装界面
- ✅ 自定义安装目录
- ✅ 安装进度显示
- ✅ 桌面快捷方式创建选项
- ✅ 开始菜单快捷方式创建选项
- ✅ 安装完成后可立即运行程序
- ✅ 支持自定义压缩级别

## 系统要求

- Windows 7/8/10/11
- Go 1.18 或更高版本

## 编译步骤

### 1. 安装依赖

```powershell
go get github.com/lxn/walk
```

### 2. 编译安装程序

使用提供的测试脚本：

```powershell
.	est_gui.ps1
```

或者手动编译：

```powershell
# 编译stub（带GUI）
go build -o installer/stub/stub.exe ./installer/stub

# 编译主打包程序
go build -o main.exe
```

## 使用方法

### 1. 准备你的应用程序

将你的应用程序可执行文件命名为`yuumi.exe`，并放在项目根目录下。

### 2. 配置安装选项

编辑`main.go`文件，配置安装选项：

```go
installer.Options{
    ProductName:             "lolyuumi",           // 产品名称
    ExeName:                 "yuumi.exe",          // 主程序文件名
    CreateDesktopShortcut:   true,                  // 创建桌面快捷方式
    CreateStartMenuShortcut: true,                  // 创建开始菜单快捷方式
    Version:                 "0.9.1",              // 版本号
    ShortcutName:            "悠米助手纯净版",      // 快捷方式名称
    CompressionLevel:        gzip.BestCompression,  // 压缩级别
}
```

### 3. 生成安装程序

```powershell
./main.exe
```

这将生成一个名为`lol_yuumi_setup_v091.exe`的安装程序。

### 4. 运行安装程序

双击生成的安装程序，按照图形界面的指引完成安装：

1. 选择安装目录
2. 选择是否创建快捷方式
3. 点击"安装"按钮
4. 安装完成后可选择立即运行程序

## 压缩级别说明

你可以通过`CompressionLevel`参数设置压缩级别：

- `gzip.DefaultCompression` (0) - 默认压缩级别
- `gzip.BestSpeed` (1) - 最快压缩（最小压缩比）
- `gzip.BestCompression` (9) - 最高压缩比（最慢）

## 自定义界面

你可以修改`installer/stub/main.go`文件来自定义 GUI 界面：

- 修改窗口标题和大小
- 调整控件布局
- 更改文本内容
- 自定义颜色和样式

## 常见问题

### Q: 编译时提示找不到 walk 库？

A: 请确保在 Windows 环境下编译，并已正确安装 walk 库：`go get github.com/lxn/walk`

### Q: 安装程序运行时提示"magic mismatch"？

A: 这可能是因为 stub.exe 和你的应用程序没有正确合并，请重新运行`main.exe`生成安装程序。

### Q: 如何修改安装程序的图标？

A: 你可以使用第三方工具如 Resource Hacker 来修改生成的安装程序图标。

## 注意事项

- 该安装程序仅支持 Windows 平台
- 在 macOS/Linux 环境下编译会失败，这是正常的，因为 walk 库只支持 Windows
- 请确保你的应用程序是 32 位或 64 位的 Windows 可执行文件

## 许可证

MIT License
