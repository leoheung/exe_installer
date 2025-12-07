package main

import (
	"log"

	"exe_installer/installer"
)

func main() {
	err := installer.CreateInstaller(
		"./stub.exe",
		"./yuumi.exe",
		"./lol_yuumi_setup_v090.exe",
		installer.Options{
			ProductName:             "lolyuumi",
			ExeName:                 "yuumi.exe", // 若你的真实文件名不同，改这里
			CreateDesktopShortcut:   true,
			CreateStartMenuShortcut: true,
			Version:                 "0.9.0",
			ShortcutName:            "悠米助手纯净版",
		},
	)
	if err != nil {
		log.Fatal(err)
	}
}