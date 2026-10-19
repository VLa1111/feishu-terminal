package main

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	AppID     string
	AppSecret string
	Shell     string
	Allowlist map[string]bool
	// BotOpenID 是本机器人自己在这个应用视角下的 open_id，用来判断群消息是否 @ 了自己。
	// 留空时启动后自动调 /open-apis/bot/v3/info 获取，通常不必手动填。
	BotOpenID string

	// ExecTimeout 是一条命令"同步等待"的上限。超过后先回执，结果稍后推送。
	ExecTimeout time.Duration
	// MaxJobDuration 是后台任务跟踪的最长时间，超时后停止跟踪（不会杀进程）。
	MaxJobDuration time.Duration
	// OutputLimit 是单条消息最多显示多少字节；超过就上传完整日志文件。
	OutputLimit int
	// TmuxSocket 是 tmux 的 socket 名，和用户自己的 tmux 会话隔离。
	TmuxSocket string
	// 终端尺寸。宽一点能减少长输出折行，也影响 TUI 程序的画面。
	Cols int
	Rows int

	// UpdateURL 是新版本清单（release.json）或二进制直链地址。
	// /upgrade、/checkupdate 不带参数时用它。
	UpdateURL string
	// UpdateCheck > 0 时后台按这个间隔检查新版本，发现后通知最近活跃的聊天。
	// 0 表示只在手动 /checkupdate 时检查。
	UpdateCheck time.Duration
}

func LoadConfig() *Config {
	_ = godotenv.Load() // ignore error; env may already be exported

	cfg := &Config{
		AppID:          os.Getenv("FEISHU_APP_ID"),
		AppSecret:      os.Getenv("FEISHU_APP_SECRET"),
		Shell:          os.Getenv("FEISHU_SHELL"),
		Allowlist:      map[string]bool{},
		BotOpenID:      strings.TrimSpace(os.Getenv("FEISHU_BOT_OPEN_ID")),
		ExecTimeout:    envDuration("FEISHU_EXEC_TIMEOUT", 8*time.Second),
		MaxJobDuration: envDuration("FEISHU_MAX_JOB_DURATION", 30*time.Minute),
		OutputLimit:    envInt("FEISHU_OUTPUT_LIMIT", 3000),
		TmuxSocket:     envString("FEISHU_TMUX_SOCKET", "feishu-terminal"),
		Cols:           envInt("FEISHU_TERM_COLS", 120),
		Rows:           envInt("FEISHU_TERM_ROWS", 40),
		UpdateURL:      strings.TrimSpace(os.Getenv("FEISHU_UPDATE_URL")),
		UpdateCheck:    envDuration("FEISHU_UPDATE_CHECK", 0),
	}
	if cfg.Shell == "" {
		cfg.Shell = "/bin/zsh"
	}
	for _, id := range strings.Split(os.Getenv("FEISHU_ALLOW_OPEN_IDS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.Allowlist[id] = true
		}
	}
	return cfg
}

func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}
