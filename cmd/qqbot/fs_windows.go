//go:build windows

package main

import "errors"

// diskFreeGB 在 Windows 上不实现，返回错误让调用方跳过该项检查。
// 目标部署环境是 Linux，本地开发用不到这个能力。
func diskFreeGB(dir string) (float64, error) {
	return 0, errors.New("Windows 平台不支持磁盘空间检测")
}
