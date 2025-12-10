package main

import (
	"compress/gzip"
	"log"

	"exe_installer/installer"
)

func main() {
	err := installer.CreateInstaller(
		"./installer/stub/stub.exe",
		"./yuumi.exe",
		"./lol_yuumi_setup_v091.exe",
		installer.Options{
			ProductName:             "lolyuumi",
			ExeName:                 "yuumi.exe", // 若你的真实文件名不同，改这里
			CreateDesktopShortcut:   true,
			CreateStartMenuShortcut: true,
			Version:                 "0.9.1",
			ShortcutName:            "悠米助手纯净版",
			CompressionLevel:        gzip.BestCompression, // 使用最高压缩级别
		},
	)
	if err != nil {
		log.Fatal(err)
	}
}