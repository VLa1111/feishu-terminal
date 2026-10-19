package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

// 这三个变量由构建时注入：
//
//	go build -ldflags "-X main.version=2.0.0 -X main.commit=abc123 -X main.buildTime=2026-10-18T00:00:00Z"
//
// 直接 go build 时保持默认值，方便本地开发。
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

var startTime = time.Now()

// versionLine 是 --version 的一行输出，也是 /version 的第一行。
// 升级前的自检会检查输出的前缀，改这里要同步改 selfTest。
func versionLine() string {
	return fmt.Sprintf("feishu-terminal %s (commit %s, built %s, %s/%s)",
		version, commit, buildTime, runtime.GOOS, runtime.GOARCH)
}

// versionInfo 是 /version 的完整回显：版本、进程、运行时长、可执行文件、更新源。
func versionInfo(cfg *Config) string {
	exe, err := currentExecutable()
	if err != nil || exe == "" {
		exe = "(未知)"
	}
	updateURL := cfg.UpdateURL
	if updateURL == "" {
		updateURL = "(未设置，/upgrade 需要显式带地址)"
	}
	return fmt.Sprintf("%s\npid %d · 已运行 %s\n可执行文件: %s\n更新清单: %s",
		versionLine(), os.Getpid(), shortDur(time.Since(startTime).Round(time.Second)), exe, updateURL)
}
