package main

import (
	"exe_installer/installer"
	"fmt"
)

func main() {
	filename, _ := installer.DownloadFileByConcurrent("https://pyq.memomind.cn/yuumiapp", "./", 8)
	fmt.Println(filename)
	installer.InstallApp(fmt.Sprintf("./%s", filename), "./build", "YMZS", "yuumi.exe", true, true)
}
