package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

// 在线升级：从清单/直链下载新二进制 → 校验 SHA-256 → 自检 → 备份 → 原子替换 → exec 重启。
//
// 设计要点：
//   - 临时文件建在可执行文件同目录，保证 os.Rename 不跨文件系统。
//   - 替换前先跑 `--version` 自检，坏包不会覆盖掉能跑的版本。
//   - 旧版本留在 <exe>.rollback，/rollback 可切换回来。
//   - 用 syscall.Exec 原地重启：PID、cwd、环境变量、日志 fd 都不变；
//     tmux 会话是独立进程，升级不会丢 shell。
const (
	updateMaxManifest    = 1 << 20   // 清单元数据上限
	updateMaxBinary      = 256 << 20 // 二进制上限
	updateHTTPTimeout    = 120 * time.Second
	updateSelfTestLimit  = 20 * time.Second
	updateCheckStartWait = 45 * time.Second // 启动后先让长连接稳定再检查
)

type releaseArtifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// releaseManifest 是 release.json 的结构，由 scripts/release.sh 生成。
//
//	{
//	  "version": "2.0.1",
//	  "notes": "fix ...",
//	  "built_at": "2026-10-18T09:00:00Z",
//	  "artifacts": {
//	    "linux-amd64": {"url": "feishu-terminal-linux-amd64", "sha256": "..."},
//	    "linux-arm64": {"url": "feishu-terminal-linux-arm64", "sha256": "..."}
//	  }
//	}
//
// artifacts[].url 可以是相对清单地址的相对路径，也可以是绝对 URL。
type releaseManifest struct {
	Version   string                     `json:"version"`
	Notes     string                     `json:"notes"`
	BuiltAt   string                     `json:"built_at"`
	Artifacts map[string]releaseArtifact `json:"artifacts"`
}

func artifactKey() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// isManifestURL 判断地址指向 JSON 清单而不是裸二进制。
func isManifestURL(u string) bool {
	p := u
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	return strings.HasSuffix(strings.ToLower(p), ".json")
}

// resolveArtifactURL 把清单里的相对地址按清单地址解析成绝对地址。
func resolveArtifactURL(manifestURL, ref string) string {
	b, err := url.Parse(manifestURL)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// ---- 版本比较 ----

// parseVer 解析 "v2.1.0" / "2.1" / "2.1.0-rc1"；"dev" 或无法解析时返回 ok=false。
func parseVer(s string) ([]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" || s == "dev" {
		return nil, false
	}
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		nums = append(nums, n)
	}
	return nums, true
}

// compareVersions 返回 -1 / 0 / 1。无法解析的版本（如 dev）视为比任何发行版都旧。
func compareVersions(a, b string) int {
	pa, oka := parseVer(a)
	pb, okb := parseVer(b)
	switch {
	case !oka && !okb:
		return strings.Compare(a, b)
	case !oka:
		return -1
	case !okb:
		return 1
	}
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ---- 参数解析 ----

// parseUpgradeArgs 解析 /upgrade 后面的一段文本：
//
//	<url> [sha256] [--force]
func parseUpgradeArgs(s string) (rawURL, sha string, force bool) {
	for _, f := range strings.Fields(s) {
		switch {
		case f == "--force" || f == "-f":
			force = true
		case len(f) == 64 && isHex(f):
			sha = f
		default:
			if rawURL == "" {
				rawURL = f
			}
		}
	}
	return
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return s != ""
}

// ---- 网络 ----

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: updateHTTPTimeout}
}

func newHTTPRequest(ctx context.Context, u string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "feishu-terminal/"+version)
	return req, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// httpGetBytes 取小块内容（清单），失败重试 2 次；4xx 不重试。
func httpGetBytes(ctx context.Context, u string, limit int64) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			if err := sleepCtx(ctx, time.Duration(attempt-1)*time.Second); err != nil {
				return nil, err
			}
		}
		req, err := newHTTPRequest(ctx, u)
		if err != nil {
			return nil, err
		}
		resp, err := newHTTPClient().Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, fmt.Errorf("HTTP %s（地址或权限不对）", resp.Status)
		}
		if resp.StatusCode/100 != 2 {
			lastErr = fmt.Errorf("HTTP %s", resp.Status)
			continue
		}
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if int64(len(body)) > limit {
			return nil, fmt.Errorf("响应超过 %d 字节上限", limit)
		}
		return body, nil
	}
	return nil, lastErr
}

func fetchManifest(ctx context.Context, u string) (*releaseManifest, error) {
	body, err := httpGetBytes(ctx, u, updateMaxManifest)
	if err != nil {
		return nil, err
	}
	var m releaseManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("解析版本清单失败: %w", err)
	}
	m.Version = strings.TrimSpace(m.Version)
	if m.Version == "" {
		return nil, errors.New("版本清单缺少 version 字段")
	}
	if len(m.Artifacts) == 0 {
		return nil, errors.New("版本清单缺少 artifacts 字段")
	}
	return &m, nil
}

// downloadOnce 下载到 dir 下的临时文件，同时算 SHA-256。
func downloadOnce(ctx context.Context, u, dir string, limit int64) (path, sum string, err error) {
	req, err := newHTTPRequest(ctx, u)
	if err != nil {
		return "", "", err
	}
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", "", fmt.Errorf("HTTP %s", resp.Status)
	}

	f, err := os.CreateTemp(dir, ".feishu-terminal-update-*")
	if err != nil {
		return "", "", fmt.Errorf("在 %s 创建临时文件失败（检查写权限）: %w", dir, err)
	}
	name := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(name)
		}
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", "", fmt.Errorf("下载中断: %w", err)
	}
	if n > limit {
		return "", "", fmt.Errorf("文件超过 %d 字节上限", limit)
	}
	if n == 0 {
		return "", "", errors.New("下载内容为空")
	}
	if err := f.Sync(); err != nil {
		return "", "", err
	}
	if err := f.Close(); err != nil {
		return "", "", err
	}
	ok = true
	return name, hex.EncodeToString(h.Sum(nil)), nil
}

func downloadWithRetry(ctx context.Context, u, dir string, limit int64) (string, string, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			if err := sleepCtx(ctx, time.Duration(attempt-1)*time.Second); err != nil {
				return "", "", err
			}
		}
		p, sum, err := downloadOnce(ctx, u, dir, limit)
		if err == nil {
			return p, sum, nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "写权限") {
			return "", "", err // 重试也没用
		}
	}
	return "", "", lastErr
}

// ---- 本地文件 ----

func currentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// copyFile 原子地写 dst（先写同目录临时文件再 rename）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := fmt.Sprintf("%s.tmp-%d", dst, os.Getpid())
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// selfTest 用新二进制跑 `--version`，返回它的版本行。
// 这是最后一道防线：架构不对、动态库缺失、文件损坏都会在这里暴露。
func selfTest(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), updateSelfTestLimit)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("新程序自检失败: %v（输出: %s）", err, clip(s, 300))
	}
	if !strings.HasPrefix(s, "feishu-terminal ") {
		return "", fmt.Errorf("自检输出异常: %s", clip(s, 300))
	}
	return s, nil
}

// installBinary 备份当前版本再原子替换。newPath 必须与目标同目录。
func installBinary(newPath, exe string) error {
	if err := os.Chmod(newPath, 0o755); err != nil {
		return err
	}
	backup := exe + ".rollback"
	if err := copyFile(exe, backup); err != nil {
		// 备份失败不阻断升级，但回滚会不可用；日志里说清楚。
		log.Printf("备份当前版本到 %s 失败（/rollback 将不可用）: %v", backup, err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		return fmt.Errorf("替换 %s 失败（检查目录写权限）: %w", exe, err)
	}
	return nil
}

func reexecSelf() error {
	exe, err := currentExecutable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}

// ---- 升级流程 ----

var upgradeMu sync.Mutex

// performUpgrade 完成下载→校验→自检→替换。
// 返回 (提示信息, 是否真的替换了文件, 错误)。
func performUpgrade(ctx context.Context, cfg *Config, arg string) (string, bool, error) {
	rawURL, shaWant, force := parseUpgradeArgs(arg)
	if rawURL == "" {
		rawURL = cfg.UpdateURL
	}
	if rawURL == "" {
		return "", false, errors.New("未指定更新地址。用法: /upgrade <release.json 或二进制 URL> [sha256] [--force]；也可在 .env 里设置 FEISHU_UPDATE_URL")
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return "", false, fmt.Errorf("只支持 http/https 地址: %s", rawURL)
	}

	exe, err := currentExecutable()
	if err != nil {
		return "", false, err
	}
	dir := filepath.Dir(exe)

	if isManifestURL(rawURL) {
		m, err := fetchManifest(ctx, rawURL)
		if err != nil {
			return "", false, fmt.Errorf("获取版本清单失败: %w", err)
		}
		if !force && compareVersions(m.Version, version) <= 0 {
			return fmt.Sprintf("已是最新版本 %s（远端 %s）。要强制重装加 --force。", version, m.Version), false, nil
		}
		art, ok := m.Artifacts[artifactKey()]
		if !ok {
			keys := make([]string, 0, len(m.Artifacts))
			for k := range m.Artifacts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return "", false, fmt.Errorf("清单里没有本机架构 %s（清单提供: %s）", artifactKey(), strings.Join(keys, ", "))
		}
		log.Printf("升级：清单 %s 版本 %s → 构件 %s", rawURL, m.Version, art.URL)
		rawURL = resolveArtifactURL(rawURL, art.URL)
		shaWant = strings.TrimSpace(art.SHA256)
	}

	tmp, got, err := downloadWithRetry(ctx, rawURL, dir, updateMaxBinary)
	if err != nil {
		return "", false, fmt.Errorf("下载失败: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmp)
		}
	}()

	if shaWant == "" {
		log.Printf("升级：地址未提供 sha256，跳过哈希校验（%s）", rawURL)
	} else if !strings.EqualFold(got, shaWant) {
		return "", false, fmt.Errorf("校验失败，已放弃升级：期望 %s，实际 %s", shaWant, got)
	}

	// os.CreateTemp 建出来是 0600，必须先给执行位，否则自检会 permission denied。
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", false, err
	}
	verLine, err := selfTest(tmp)
	if err != nil {
		return "", false, err
	}
	if err := installBinary(tmp, exe); err != nil {
		return "", false, err
	}
	keep = true // rename 成功后 tmp 已不存在，这里只是不再 Remove

	msg := fmt.Sprintf("升级完成：%s → %s\n校验: sha256 %s", version, verLine, got)
	if shaWant == "" {
		msg += "（未校验）"
	}
	return msg, true, nil
}

// ---- 飞书命令入口 ----

func handleUpgradeCommand(client *lark.Client, cfg *Config, receiveID, receiveIDType, arg string) {
	if !upgradeMu.TryLock() {
		reply(client, receiveID, receiveIDType, "已有升级/回滚在进行中，请稍候。")
		return
	}
	go func() {
		defer upgradeMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()

		reply(client, receiveID, receiveIDType,
			"开始升级：下载 → SHA-256 校验 → 自检 → 原子替换。完成后自动重启，tmux 会话不受影响。")
		msg, updated, err := performUpgrade(ctx, cfg, arg)
		if err != nil {
			log.Printf("升级失败: %v", err)
			reply(client, receiveID, receiveIDType, "升级失败，当前版本未变：\n"+err.Error())
			return
		}
		reply(client, receiveID, receiveIDType, msg)
		if !updated {
			return
		}
		time.Sleep(800 * time.Millisecond) // 给消息发送留一点余量
		reply(client, receiveID, receiveIDType, "正在重启...")
		time.Sleep(300 * time.Millisecond)
		if err := reexecSelf(); err != nil {
			log.Printf("重启失败: %v", err)
			reply(client, receiveID, receiveIDType, "升级已完成，但自动重启失败，请手动重启进程：\n"+err.Error())
		}
	}()
}

func handleRollbackCommand(client *lark.Client, receiveID, receiveIDType string) {
	if !upgradeMu.TryLock() {
		reply(client, receiveID, receiveIDType, "已有升级/回滚在进行中，请稍候。")
		return
	}
	go func() {
		defer upgradeMu.Unlock()

		exe, err := currentExecutable()
		if err != nil {
			reply(client, receiveID, receiveIDType, "回滚失败: "+err.Error())
			return
		}
		backup := exe + ".rollback"
		if _, err := os.Stat(backup); err != nil {
			reply(client, receiveID, receiveIDType, "没有可回滚的版本（从未通过 /upgrade 升级过）。")
			return
		}
		verLine, err := selfTest(backup)
		if err != nil {
			reply(client, receiveID, receiveIDType, "回滚失败，备份版本不可用：\n"+err.Error())
			return
		}
		// 当前版本先留到 .rollback.tmp，再把备份换上来，最后把当前版本存成新的 .rollback。
		// 于是连续 /rollback 可以在两个版本之间来回切换。
		tmp := exe + ".rollback.tmp"
		if err := copyFile(exe, tmp); err != nil {
			reply(client, receiveID, receiveIDType, "回滚失败（保存当前版本）: "+err.Error())
			return
		}
		if err := os.Rename(backup, exe); err != nil {
			_ = os.Remove(tmp)
			reply(client, receiveID, receiveIDType, "回滚失败（替换可执行文件）: "+err.Error())
			return
		}
		if err := os.Rename(tmp, backup); err != nil {
			log.Printf("保存被替换版本失败（不影响回滚）: %v", err)
		}

		reply(client, receiveID, receiveIDType,
			"已回滚：\n"+verLine+"\n正在重启（当前版本已保存，可再次 /rollback 切回）...")
		time.Sleep(800 * time.Millisecond)
		if err := reexecSelf(); err != nil {
			log.Printf("重启失败: %v", err)
			reply(client, receiveID, receiveIDType, "回滚已完成，但自动重启失败，请手动重启进程：\n"+err.Error())
		}
	}()
}

// handleCheckUpdate 检查清单里的版本。参数可选，缺省用 FEISHU_UPDATE_URL。
func handleCheckUpdate(client *lark.Client, cfg *Config, receiveID, receiveIDType, arg string) {
	rawURL := strings.TrimSpace(arg)
	if rawURL == "" {
		rawURL = cfg.UpdateURL
	}
	if rawURL == "" {
		reply(client, receiveID, receiveIDType,
			"用法: /checkupdate <release.json 地址>；或在 .env 里设置 FEISHU_UPDATE_URL 后直接 /checkupdate")
		return
	}
	if !isManifestURL(rawURL) {
		reply(client, receiveID, receiveIDType, "检查更新需要指向版本清单 release.json，而不是二进制直链。")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		m, err := fetchManifest(ctx, rawURL)
		if err != nil {
			reply(client, receiveID, receiveIDType, "检查更新失败: "+err.Error())
			return
		}
		if compareVersions(m.Version, version) > 0 {
			msg := fmt.Sprintf("发现新版本 %s（当前 %s）。发 /upgrade 升级。", m.Version, version)
			if m.Notes != "" {
				msg += "\n" + m.Notes
			}
			reply(client, receiveID, receiveIDType, msg)
			return
		}
		reply(client, receiveID, receiveIDType,
			fmt.Sprintf("已是最新（当前 %s，远端 %s）。", version, m.Version))
	}()
}

// ---- 后台检查 ----

// 最近一次与白名单用户交互的聊天，用于主动推送新版本通知。
var (
	lastChatMu   sync.Mutex
	lastChatID   string
	lastChatType string
)

func rememberChat(id, typ string) {
	if id == "" {
		return
	}
	lastChatMu.Lock()
	lastChatID, lastChatType = id, typ
	lastChatMu.Unlock()
}

func currentChat() (string, string) {
	lastChatMu.Lock()
	defer lastChatMu.Unlock()
	return lastChatID, lastChatType
}

// updateCheckLoop 周期检查清单，发现新版本时通知最近活跃的聊天。
// 同一版本只通知一次；需要 FEISHU_UPDATE_CHECK > 0 才启用。
func updateCheckLoop(client *lark.Client, cfg *Config) {
	notified := ""
	check := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		m, err := fetchManifest(ctx, cfg.UpdateURL)
		if err != nil {
			log.Printf("检查新版本失败: %v", err)
			return
		}
		if compareVersions(m.Version, version) <= 0 {
			notified = ""
			return
		}
		if notified == m.Version {
			return
		}
		notified = m.Version
		msg := fmt.Sprintf("发现新版本 %s（当前 %s）。发 /upgrade 升级。", m.Version, version)
		if m.Notes != "" {
			msg += "\n" + m.Notes
		}
		log.Printf("%s", msg)
		if id, typ := currentChat(); id != "" {
			reply(client, id, typ, msg)
		}
	}

	time.Sleep(updateCheckStartWait)
	check()
	t := time.NewTicker(cfg.UpdateCheck)
	defer t.Stop()
	for range t.C {
		check()
	}
}
