package main

import (
	"compress/gzip"
	"log"

	"exe_installer/installer"
)

func main() {
	err := installer.CreateInstaller(
		"./installer/stub/stub.exe",
		"", // 不再需要本地exe文件，改为从URL下载
		"./lol_yuumi_setup.exe",
		installer.Options{
			ProductName:             "lolyuumi",
			ExeName:                 "yuumi.exe", // 下载后保存的文件名
			CreateDesktopShortcut:   true,
			CreateStartMenuShortcut: true,
			Version:                 "latest",
			ShortcutName:            "悠米助手纯净版",
			CompressionLevel:        gzip.BestCompression,               // 使用最高压缩级别
			DownloadURL:             "https://pyq.memomind.cn/yuumiapp", // 下载URL
			NumWorkers:              8,                                  // 并发下载线程数
		},
	)
	if err != nil {
		log.Fatal(err)
	}
}
