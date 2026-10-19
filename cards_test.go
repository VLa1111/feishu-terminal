package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

func TestParseWatchArgs(t *testing.T) {
	cases := []struct {
		in        string
		wantCmd   string
		wantEvery time.Duration
		wantFor   time.Duration
		wantStop  bool
		wantErr   bool
	}{
		{in: "/watch top", wantCmd: "top", wantEvery: 2 * time.Second, wantFor: 5 * time.Minute},
		{in: "/watch --every 3s --for 1m top -d 1", wantCmd: "top -d 1", wantEvery: 3 * time.Second, wantFor: time.Minute},
		{in: "/watch stop", wantStop: true, wantEvery: 2 * time.Second, wantFor: 5 * time.Minute},
		{in: "/watch", wantCmd: "", wantEvery: 2 * time.Second, wantFor: 5 * time.Minute},
		{in: "/watch top --every 800ms", wantEvery: time.Second, wantFor: 5 * time.Minute, wantCmd: "top"}, // 下限 1s
		{in: "/watch top --every 100ms", wantErr: true},                                                    // 太快直接拒绝
		{in: "/watch top --for 2h", wantEvery: 2 * time.Second, wantFor: 30 * time.Minute, wantCmd: "top"}, // 上限 30m
		{in: "/watch --every", wantErr: true},
		{in: "/watch --for abc top", wantErr: true},
	}
	for _, c := range cases {
		cmd, every, dur, stop, err := parseWatchArgs(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: 期望报错，实际得到 cmd=%q every=%v for=%v", c.in, cmd, every, dur)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: 意外报错: %v", c.in, err)
			continue
		}
		if cmd != c.wantCmd || every != c.wantEvery || dur != c.wantFor || stop != c.wantStop {
			t.Errorf("%q: 得到 cmd=%q every=%v for=%v stop=%v，期望 cmd=%q every=%v for=%v stop=%v",
				c.in, cmd, every, dur, stop, c.wantCmd, c.wantEvery, c.wantFor, c.wantStop)
		}
	}
}

// 卡片必须是合法 JSON，且不超过飞书 30KB 的上限。
func TestCardJSONValid(t *testing.T) {
	job := &Job{ID: 7, Cmd: "apt install -y nginx"}
	cards := map[string]string{
		"prompt": promptCard("chat_x", "chat_id", "chat_x", job, "apt"),
		"watch":  watchCard("top", "line1\nline2", "chat_x", "chat_id", "chat_x", true),
		"stop":   stoppedCard("top", "已停止", "line1\nline2"),
		"help":   helpCard("chat_x", "chat_id", "chat_x"),
		"done":   doneCard(job, 0),
	}
	for name, c := range cards {
		if len(c) > 30*1024 {
			t.Errorf("%s 卡片 %d 字节，超过 30KB", name, len(c))
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(c), &v); err != nil {
			t.Errorf("%s 卡片不是合法 JSON: %v", name, err)
			continue
		}
		if _, ok := v["elements"]; !ok {
			t.Errorf("%s 卡片缺少 elements", name)
		}
	}
}

// waitBusy 等终端进入/退出忙碌状态，避免 tmux 的异步性造成竞态。
func waitBusy(t *testing.T, s *Session, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.Busy() == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	snap, _ := s.Snapshot(10)
	t.Fatalf("等待 Busy()==%v 超时，画面:\n%s", want, snap)
}

func cardEvent(openID, key, act, text string) *callback.CardActionTriggerEvent {
	uid := ""
	return &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: openID, UserID: &uid},
			Action: &callback.CallBackAction{
				Value: map[string]any{"key": key, "act": act, "text": text, "rt": "chat_id", "rid": key},
			},
		},
	}
}

// 卡片按钮的回调路径：白名单 + 路由到对应会话 + 真正写进终端。
func TestCardActionInputAndKey(t *testing.T) {
	m := testManager(t)
	s, err := m.GetOrCreate("chat-card")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	cfg := &Config{Allowlist: map[string]bool{"ou_test": true}}

	// 未授权的人按按钮必须被拒绝，且不能写入终端。
	if err := s.RunRaw("read x"); err != nil {
		t.Fatalf("RunRaw: %v", err)
	}
	waitBusy(t, s, true, 3*time.Second)
	if resp := handleCardAction(nil, m, cfg, cardEvent("ou_intruder", "chat-card", "input", "pwned")); resp.Toast == nil || resp.Toast.Type != "error" {
		t.Fatalf("未授权操作应返回 error toast，实际: %+v", resp.Toast)
	}
	if !s.Busy() {
		t.Fatalf("未授权操作不应让 read 结束")
	}

	// 授权用户点 y：输入进入前台进程，read 结束回到提示符。
	if resp := handleCardAction(nil, m, cfg, cardEvent("ou_test", "chat-card", "input", "hello")); resp.Toast == nil || resp.Toast.Type != "success" {
		t.Fatalf("输入按钮应返回 success toast，实际: %+v", resp.Toast)
	}
	waitBusy(t, s, false, 3*time.Second)

	// Ctrl-C 按钮中断前台进程。
	if err := s.RunRaw("sleep 30"); err != nil {
		t.Fatalf("RunRaw sleep: %v", err)
	}
	waitBusy(t, s, true, 3*time.Second)
	if resp := handleCardAction(nil, m, cfg, cardEvent("ou_test", "chat-card", "key", "ctrl-c")); resp.Toast == nil || resp.Toast.Type != "success" {
		t.Fatalf("Ctrl-C 按钮应返回 success toast，实际: %+v", resp.Toast)
	}
	waitBusy(t, s, false, 5*time.Second)
}

func TestHelpSpecs(t *testing.T) {
	cfg := &Config{ExecTimeout: 8 * time.Second, MaxJobDuration: 30 * time.Minute, OutputLimit: 3000}
	spec := helpSpec(cfg)
	if len(spec) == 0 || len(spec) > 8000 {
		t.Fatalf("AI 规格长度异常: %d 字节", len(spec))
	}
	// agent 必须能从规格里看到所有命令与关键约束。
	for _, want := range []string{
		"/help ai", "/help json", "/screen", "/key", "/raw", "/watch", "/jobs", "/reset",
		"/version", "/checkupdate", "/upgrade", "/rollback",
		"stdin", "docker attach", "8s", "30m", "3000",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("AI 规格缺少 %q", want)
		}
	}

	var v map[string]any
	if err := json.Unmarshal([]byte(helpSpecJSON(cfg)), &v); err != nil {
		t.Fatalf("helpSpecJSON 不是合法 JSON: %v", err)
	}
	if v["name"] != "feishu-terminal" {
		t.Errorf("JSON 规格 name = %v", v["name"])
	}
	if !strings.Contains(helpText(), "/watch") {
		t.Errorf("人类帮助缺少 /watch")
	}
}

func TestShortDur(t *testing.T) {
	cases := map[time.Duration]string{
		8 * time.Second:         "8s",
		30 * time.Minute:        "30m",
		90 * time.Second:        "90s",
		1500 * time.Millisecond: "1.5s",
	}
	for d, want := range cases {
		if got := shortDur(d); got != want {
			t.Errorf("shortDur(%v) = %q, 期望 %q", d, got, want)
		}
	}
}
