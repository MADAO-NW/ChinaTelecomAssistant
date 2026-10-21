// Package version 记录构建版本信息。
//
// 发布流水线通过 -ldflags -X 注入：
//
//	-X ctm/internal/version.Version=0.1.0 -X ctm/internal/version.BuildType=release
//
// 本地/开发构建保持 source，不参与“是否最新版”的比较。
package version

// Version 当前版本号（不含 v 前缀，例如 0.1.0）。
var Version = "dev"

// BuildType 构建来源：release（CI 发布）或 source（本地构建）。
var BuildType = "source"

// Current 返回当前版本号。
func Current() string { return Version }

// IsRelease 是否为发布构建；只有发布构建参与版本比较。
func IsRelease() bool {
	return BuildType == "release" && Version != "" && Version != "dev"
}
