package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testManager 每个测试用独立的 tmux socket，避免和真实 bot 的会话互相干扰。
func testManager(t *testing.T) *SessionManager {
	t.Helper()
	zshDir, _ := filepath.Abs("zsh")
	sock := fmt.Sprintf("fttest-%d-%d", os.Getpid(), time.Now().UnixNano())
	m := NewSessionManager("/bin/zsh", zshDir, sock, t.TempDir(), 100, 30)
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
	})
	return m
}

func mustRun(t *testing.T, s *Session, cmd string, timeout time.Duration) (string, int) {
	t.Helper()
	job, err := s.Run(cmd)
	if err != nil {
		t.Fatalf("Run(%q): %v", cmd, err)
	}
	if !s.Wait(job, timeout) {
		snap, _ := s.Snapshot(20)
		t.Fatalf("命令 %q 在 %s 内未结束，画面:\n%s", cmd, timeout, snap)
	}
	return s.ReadResult(job)
}

func TestSessionStatePersistence(t *testing.T) {
	m := testManager(t)
	s, err := m.GetOrCreate("chat-1")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	if _, code := mustRun(t, s, "cd /tmp", 8*time.Second); code != 0 {
		t.Fatalf("cd 退出码 = %d", code)
	}
	out, code := mustRun(t, s, "pwd", 8*time.Second)
	if code != 0 {
		t.Fatalf("pwd 退出码 = %d", code)
	}
	if !strings.Contains(out, "/tmp") {
		t.Fatalf("pwd 输出应含 /tmp（会话状态没保留），实际: %q", out)
	}
	if p := s.CurrentPrompt(); !strings.Contains(p, "/tmp") {
		t.Fatalf("提示符应显示 /tmp，实际: %q", p)
	}

	// 同一个 key 应复用同一个 tmux 会话。
	if s2, err := m.GetOrCreate("chat-1"); err != nil || s2.name != s.name {
		t.Fatalf("同一 key 未复用会话: %v / %v", err, s2)
	}
}

func TestJobExitCodeAndLongOutput(t *testing.T) {
	m := testManager(t)
	s, err := m.GetOrCreate("chat-2")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	out, code := mustRun(t, s, "(exit 3)", 8*time.Second)
	if code != 3 {
		t.Fatalf("退出码应为 3，实际 %d（输出 %q）", code, out)
	}

	out, code = mustRun(t, s, "seq 1 2000", 15*time.Second)
	if code != 0 {
		t.Fatalf("seq 退出码 = %d", code)
	}
	if len(out) < 3000 {
		t.Fatalf("长输出被意外截断，长度 %d", len(out))
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "2000") {
		t.Fatalf("长输出结尾不完整，末尾: %q", tailRunes(out, 40))
	}

	// stderr 也要进同一个输出文件。
	out, _ = mustRun(t, s, "echo oops >&2", 8*time.Second)
	if !strings.Contains(out, "oops") {
		t.Fatalf("stderr 未捕获，实际: %q", out)
	}
}

func TestInteractiveInput(t *testing.T) {
	m := testManager(t)
	s, err := m.GetOrCreate("chat-3")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	job, err := s.Run("read x; echo GOT:$x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.Wait(job, 1*time.Second) {
		t.Fatal("read 在等待输入，不应判定为已结束")
	}
	if !s.Busy() {
		t.Fatal("read 阻塞时 Busy 应为 true")
	}

	if err := s.SendInput("yes"); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if !s.Wait(job, 5*time.Second) {
		t.Fatal("输入后命令应结束")
	}
	out, code := s.ReadResult(job)
	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(out, "GOT:yes") {
		t.Fatalf("输出应含 GOT:yes，实际: %q", out)
	}
}

func TestRawModeAndCtrlC(t *testing.T) {
	m := testManager(t)
	s, err := m.GetOrCreate("chat-4")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	if err := s.RunRaw("sleep 30"); err != nil {
		t.Fatalf("RunRaw: %v", err)
	}
	if err := s.RunRaw("true"); err != ErrBusy {
		t.Fatalf("前台忙时应返回 ErrBusy，实际: %v", err)
	}
	if !s.Busy() {
		t.Fatal("sleep 运行期间 Busy 应为 true")
	}
	if snap, err := s.Snapshot(20); err != nil || snap == "" {
		t.Fatalf("Snapshot 应有内容: %q %v", snap, err)
	}

	if err := s.SendKey("ctrl-c"); err != nil {
		t.Fatalf("SendKey: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && s.Busy() {
		time.Sleep(100 * time.Millisecond)
	}
	if s.Busy() {
		snap, _ := s.Snapshot(20)
		t.Fatalf("Ctrl-C 后应回到提示符，画面:\n%s", snap)
	}
}

func TestResetKillsSession(t *testing.T) {
	m := testManager(t)
	s, err := m.GetOrCreate("chat-5")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	mustRun(t, s, "echo alive", 8*time.Second)

	m.Close("chat-5")
	if _, err := exec.Command("tmux", "-L", s.socket, "has-session", "-t", s.name).Output(); err == nil {
		t.Fatal("Close 后 tmux 会话应该已经不存在")
	}
}
