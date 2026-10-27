// Package web 内嵌前端构建产物。
//
// 用 embed 把 dist 目录打进二进制，交付物只有一个可执行文件，
// 部署时不需要额外拷贝静态资源，也不会出现"忘了传 dist"的故障。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Assets 返回前端的只读文件系统。
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// dist 目录由 embed 保证存在，走到这里说明构建配置错了
		panic("web: 无法定位 dist 目录: " + err.Error())
	}
	return sub
}
