package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// promptSentinel 会被写进 zsh 的 PROMPT。它出现在终端最后一行，就说明 shell 空闲、可以接收下一条命令。
const promptSentinel = "__FEISHU_TERM_PROMPT__"

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*(\x07|\x1b\\)|\x1b[()][A-Za-z0-9]`)
	jobSeq uint64
)

// ErrBusy 表示当前会话前台还有进程在跑，不能开始新命令。
var ErrBusy = errors.New("当前有命令正在运行")

// Job 是一条已下发命令的跟踪记录。
//
// 命令不是直接从 PTY 读输出，而是让它把 stdout/stderr 重定向到文件：
// 这样拿到的是干净、完整、无回显的文本，也天然绕开了 PTY 的行宽折行。
// 命令是否结束，只看 .rc 文件是否出现（shell 在命令后写退出码）。
type Job struct {
	ID      int
	Cmd     string
	OutPath string
	RCPath  string
	Started time.Time

	// fallback 表示"没拿到退出码，但 shell 已回到提示符"。
	mu       sync.Mutex
	fallback bool
	cardID   string // 超时回执卡片的 message_id；命令结束时用它把按钮去掉
}

type Session struct {
	mu      sync.Mutex
	name    string // tmux 会话名
	socket  string // tmux -L 的 socket 名，与用户自己的 tmux 隔离
	shell   string
	zshDir  string
	workDir string
	cwd     string
	cols    int
	rows    int

	current *Job
}

type SessionManager struct {
	mu      sync.Mutex
	sess    map[string]*Session
	shell   string
	zshDir  string
	socket  string
	workDir string
	cwd     string
	cols    int
	rows    int
}

func NewSessionManager(shell, zshDir, socket, workDir string, cols, rows int) *SessionManager {
	cwd, _ := os.Getwd()
	m := &SessionManager{
		sess:    map[string]*Session{},
		shell:   shell,
		zshDir:  zshDir,
		socket:  socket,
		workDir: workDir,
		cwd:     cwd,
		cols:    cols,
		rows:    rows,
	}
	_ = os.MkdirAll(workDir, 0o700)
	return m
}

// sessionName 把聊天 key 映射成稳定的 tmux 会话名。
// 用哈希而不是原样拼接：chat_id/open_id 里可能有 tmux 不允许的字符，
// 而且稳定映射能让 bot 重启后重新接上同一个 shell。
func sessionName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "ft_" + hex.EncodeToString(sum[:])[:12]
}

func (m *SessionManager) GetOrCreate(key string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sess[key]; ok {
		if err := s.ensure(); err == nil {
			return s, nil
		}
	}
	s := &Session{
		name:    sessionName(key),
		socket:  m.socket,
		shell:   m.shell,
		zshDir:  m.zshDir,
		workDir: m.workDir,
		cwd:     m.cwd,
		cols:    m.cols,
		rows:    m.rows,
	}
	if err := s.ensure(); err != nil {
		return nil, err
	}
	m.sess[key] = s
	return s, nil
}

// Lookup 返回已存在的会话，不创建。卡片回调按 key 路由时用它。
func (m *SessionManager) Lookup(key string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sess[key]
}

// Close 结束该聊天对应的 tmux 会话（/reset 用）。
func (m *SessionManager) Close(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sess[key]; ok {
		s.kill()
		delete(m.sess, key)
	}
}

func (s *Session) ensure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureLocked()
}

func (s *Session) ensureLocked() error {
	if _, err := s.tmux("has-session", "-t", s.name); err == nil {
		return nil
	}
	cmdline := fmt.Sprintf("env ZDOTDIR=%s FEISHU_SENTINEL=%s DISABLE_AUTO_UPDATE=true %s -i",
		shQuote(s.zshDir), shQuote(promptSentinel), shQuote(s.shell))
	if _, err := s.tmux("new-session", "-d", "-s", s.name,
		"-x", strconv.Itoa(s.cols), "-y", strconv.Itoa(s.rows),
		"-c", s.cwd, cmdline); err != nil {
		return fmt.Errorf("创建 tmux 会话失败: %w", err)
	}
	_, _ = s.tmux("set-option", "-g", "history-limit", "50000")
	// 等 shell 把提示符画出来，避免第一条命令打在未初始化的终端上。
	// 超时说明 shell 没起来（zsh 缺失、zshrc 报错等），必须报错而不是静默放行，
	// 否则后续每条命令都会以「当前有命令正在运行」失败，掩盖真正的原因。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if s.atPromptLocked() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	snap, capErr := s.captureLocked(40)
	if capErr != nil {
		return fmt.Errorf("shell 启动失败（会话 %s，ZDOTDIR=%s）: %v", s.name, s.zshDir, capErr)
	}
	return fmt.Errorf("shell 在 8s 内没有出现提示符（会话 %s，ZDOTDIR=%s），最后画面:\n%s", s.name, s.zshDir, snap)
}

func (s *Session) kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.tmux("kill-session", "-t", s.name)
	s.current = nil
}

// tmux 执行一条 tmux 命令，返回 stdout。
func (s *Session) tmux(args ...string) (string, error) {
	full := append([]string{"-L", s.socket}, args...)
	var stderr strings.Builder
	cmd := exec.Command("tmux", full...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %v %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func (s *Session) sendLiteralLocked(text string) error {
	if _, err := s.tmux("send-keys", "-l", "-t", s.name, "--", text); err != nil {
		return err
	}
	return nil
}

func (s *Session) sendEnterLocked() error {
	_, err := s.tmux("send-keys", "-t", s.name, "Enter")
	return err
}

// Busy 判断前台是否还有进程在跑。
// 两道判据：有未完成的 Job，或者终端最后一行不是带哨兵的提示符。
// 后者能覆盖 read 这类 shell 内建命令——它们在前台阻塞时，tmux 的
// pane_current_command 仍然是 zsh，靠进程名判断会漏。
func (s *Session) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busyLocked()
}

func (s *Session) busyLocked() bool {
	if s.current != nil && !s.currentDone(s.current) {
		return true
	}
	return !s.atPromptLocked()
}

// captureLocked 抓取终端最后 lines 行。调用方必须已持有 s.mu。
func (s *Session) captureLocked(lines int) (string, error) {
	return s.tmux("capture-pane", "-p", "-J", "-t", s.name, "-S", fmt.Sprintf("-%d", lines))
}

// atPromptLocked 看最后一行是否以哨兵结尾。
func (s *Session) atPromptLocked() bool {
	out, err := s.captureLocked(40)
	if err != nil {
		return false
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimRight(lines[i], " \t"); l != "" {
			// 必须是行尾：命令回显会接在提示符后面，用 Contains 会把
			// "正在执行的命令"误判成"已回到提示符"。
			return strings.HasSuffix(l, promptSentinel)
		}
	}
	return false
}

// ForegroundCommand 返回前台进程名，用于给用户提示（例如 "apt"、"python3"）。
func (s *Session) ForegroundCommand() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := s.tmux("display", "-p", "-t", s.name, "#{pane_current_command}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Run 下发一条命令并等待它结束（调用方决定等多久）。
//
// 包装方式：
//
//	{ <cmd> ; } > out 2>&1; echo $? > rc
//
// stdout/stderr 写文件，输出因此干净完整；stdin 仍然是终端，
// 所以 read / apt 这类"等用户输入"的命令依然能收到后续消息。
func (s *Session) Run(cmd string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLocked(); err != nil {
		return nil, err
	}
	if s.busyLocked() {
		return nil, ErrBusy
	}

	id := int(atomic.AddUint64(&jobSeq, 1))
	stamp := fmt.Sprintf("%d_%d", id, time.Now().UnixNano())
	j := &Job{
		ID:      id,
		Cmd:     cmd,
		OutPath: filepath.Join(s.workDir, "job_"+stamp+".out"),
		RCPath:  filepath.Join(s.workDir, "job_"+stamp+".rc"),
	}

	// 退出码先写临时文件再原子改名：直接 `> rc` 会让 Stat 在文件刚被创建、
	// 内容还没写入时就看到它，读出来是空串。
	trimmed := strings.TrimRight(strings.TrimSpace(cmd), ";")
	tmpRC := j.RCPath + ".tmp"
	line := fmt.Sprintf("{ %s ; } > %s 2>&1; echo $? > %s; mv -f %s %s",
		trimmed, shQuote(j.OutPath), shQuote(tmpRC), shQuote(tmpRC), shQuote(j.RCPath))

	if err := s.sendLiteralLocked(line); err != nil {
		return nil, err
	}
	if err := s.sendEnterLocked(); err != nil {
		return nil, err
	}
	j.Started = time.Now()
	s.current = j
	return j, nil
}

// RunRaw 把命令原样交给终端，不做输出重定向。
// 给 top/htop/vim 这类必须独占终端的程序用；用 /screen 看画面，/key 发按键。
func (s *Session) RunRaw(cmd string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLocked(); err != nil {
		return err
	}
	if s.busyLocked() {
		return ErrBusy
	}
	if err := s.sendLiteralLocked(cmd); err != nil {
		return err
	}
	return s.sendEnterLocked()
}

// SendInput 把文本写给前台进程（相当于在终端里打字回车）。
func (s *Session) SendInput(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sendLiteralLocked(text); err != nil {
		return err
	}
	return s.sendEnterLocked()
}

var keyMap = map[string]string{
	"enter": "Enter", "return": "Enter", "tab": "Tab",
	"esc": "Escape", "escape": "Escape",
	"up": "Up", "down": "Down", "left": "Left", "right": "Right",
	"ctrl-c": "C-c", "ctrlc": "C-c", "ctrl-d": "C-d", "ctrl-z": "C-z",
	"ctrl-l": "C-l", "ctrl-a": "C-a", "ctrl-e": "C-e", "ctrl-u": "C-u",
	// docker attach 的脱离序列是 Ctrl-P Ctrl-Q，不是 Ctrl-C。
	"ctrl-p": "C-p", "ctrl-q": "C-q",
	"bspace": "BSpace", "backspace": "BSpace", "space": "Space",
	"home": "Home", "end": "End", "pgup": "PPage", "pgdn": "NPage",
	"delete": "DC",
}

// SendKey 发送一个特殊按键（方向键、Ctrl-C 等）。
func (s *Session) SendKey(name string) error {
	tmuxKey, ok := keyMap[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return fmt.Errorf("不认识的按键 %q（可用: ctrl-c / ctrl-p / ctrl-q / tab / enter / esc / up / down / left / right / bspace）", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.tmux("send-keys", "-t", s.name, tmuxKey)
	return err
}

// Snapshot 返回终端画面。
// n <= 0：只看当前可见的一屏（TUI 程序应该用这个）；n > 0：连同历史共 n 行。
func (s *Session) Snapshot(n int) (string, error) {
	args := []string{"capture-pane", "-p", "-J", "-t", s.name}
	if n > 0 {
		args = append(args, "-S", strconv.Itoa(-n))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := s.tmux(args...)
	if err != nil {
		return "", err
	}
	out = cleanOutput(strings.ReplaceAll(out, promptSentinel, ""))
	out = strings.TrimRight(out, "\n")
	if n > 0 {
		lines := strings.Split(out, "\n")
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		out = strings.Join(lines, "\n")
	}
	return out, nil
}

// RunningJobInfo 返回当前未完成 Job 的简述，没有则返回空串。
func (s *Session) RunningJobInfo() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || s.currentDone(s.current) {
		return ""
	}
	j := s.current
	fg := ""
	if out, err := s.tmux("display", "-p", "-t", s.name, "#{pane_current_command}"); err == nil {
		fg = strings.TrimSpace(out)
	}
	elapsed := time.Since(j.Started).Round(time.Second)
	info := fmt.Sprintf("Job #%d（已运行 %s）: %s", j.ID, elapsed, j.Cmd)
	if fg != "" {
		info += "\n前台进程: " + fg
	}
	return info
}

// CurrentPrompt 返回当前的地址栏提示符（最后一行非空内容）。
func (s *Session) CurrentPrompt() string {
	out, err := s.Snapshot(10)
	if err != nil {
		return ""
	}
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// Wait 阻塞直到命令结束或超时。
// 返回 true 表示命令已结束（含"shell 已回到提示符但没写退出码"的兜底判定）。
func (s *Session) Wait(j *Job, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.currentDone(j) {
			return true
		}
		// 兜底：退出码文件没出现，但 shell 已经回到提示符——典型的语法错误、
		// `exec` 替换 shell、或者命令自己把 shell 关了。
		if time.Since(j.Started) > 500*time.Millisecond && s.atPrompt() {
			s.markFallback(j)
			return true
		}
		time.Sleep(80 * time.Millisecond)
	}
	return false
}

func (s *Session) atPrompt() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.atPromptLocked()
}

func (s *Session) currentDone(j *Job) bool {
	data, err := os.ReadFile(j.RCPath)
	if err != nil {
		return false
	}
	_, err = strconv.Atoi(strings.TrimSpace(string(data)))
	return err == nil
}

func (s *Session) markFallback(j *Job) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.fallback = true
}

// SetCardID 记住超时回执卡片的 message_id，命令结束后好把按钮撤掉。
func (j *Job) SetCardID(id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cardID = id
}

func (j *Job) CardID() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cardID
}

// ReadResult 读取命令输出与退出码。
func (s *Session) ReadResult(j *Job) (string, int) {
	out := ""
	if data, err := os.ReadFile(j.OutPath); err == nil {
		out = cleanOutput(string(data))
	}
	code := -1
	if data, err := os.ReadFile(j.RCPath); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			code = n
		}
	}
	j.mu.Lock()
	fallback := j.fallback
	j.mu.Unlock()
	if out == "" && fallback {
		if snap, err := s.Snapshot(40); err == nil {
			out = snap
		}
	}
	return strings.TrimRight(out, "\n"), code
}

func cleanOutput(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
