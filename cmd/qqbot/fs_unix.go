//go:build !windows

package main

import "syscall"

// diskFreeGB 返回目录所在分区的可用空间（GB）。
func diskFreeGB(dir string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return float64(st.Bavail) * float64(st.Bsize) / (1 << 30), nil
}
