// Package version 保存构建期注入的版本信息。
package version

import (
	"fmt"
	"runtime"
)

// 这些变量通过 -ldflags "-X ..." 在构建时注入，源码中保留默认值便于本地调试。
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

// String 返回一行可读的版本描述。
func String() string {
	return fmt.Sprintf("qqbot %s (commit %s, built %s, %s/%s, %s)",
		Version, Commit, BuildDate, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
