package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// 卡片按钮的 value 里带上会话 key 和回信地址，回调时才能找到对应会话。
// 群聊 key 就是 chat_id；私聊 key 是对方 open_id。
const (
	cardActInput  = "input"
	cardActKey    = "key"
	cardActScreen = "screen"
	cardActStop   = "stopwatch"
	cardActJobs   = "jobs"
	cardActAIHelp = "aihelp"

	cardSnapLimit = 6000 // 卡片里终端画面最多保留的字符数，避免超过 30KB 上限
)

// ---- 卡片 JSON 构造 ----

func cardButton(label, typ string, value map[string]any) map[string]any {
	return map[string]any{
		"tag":   "button",
		"text":  map[string]string{"tag": "plain_text", "content": label},
		"type":  typ,
		"value": value,
	}
}

func cardDiv(markdown string) map[string]any {
	return map[string]any{
		"tag":  "div",
		"text": map[string]string{"tag": "lark_md", "content": markdown},
	}
}

func cardPlain(text string) map[string]any {
	return map[string]any{
		"tag":  "div",
		"text": map[string]string{"tag": "plain_text", "content": text},
	}
}

func cardJSON(title, template string, elements []map[string]any) string {
	card := map[string]any{
		"config": map[string]any{
			"wide_screen_mode": true,
			"update_multi":     true, // 允许共享卡片被多方更新
		},
		"header": map[string]any{
			"template": template,
			"title":    map[string]string{"tag": "plain_text", "content": title},
		},
		"elements": elements,
	}
	b, _ := json.Marshal(card)
	return string(b)
}

// promptCard 在命令超时未结束时给出「进程还在跑」的卡片，附常用按键。
// 对 y/n 这类提示，点一下比打字快得多；对长构建，这些按钮至少无害。
func promptCard(key, recvType, recvID string, job *Job, fg string) string {
	if fg == "" {
		fg = "未知"
	}
	btn := func(label, typ, act, text string) map[string]any {
		v := map[string]any{"key": key, "rt": recvType, "rid": recvID, "act": act}
		if text != "" {
			v["text"] = text
		}
		return cardButton(label, typ, v)
	}

	els := []map[string]any{
		cardDiv(fmt.Sprintf("**Job #%d 仍在运行** · 前台进程: `%s`", job.ID, fg)),
		cardPlain(job.Cmd),
		cardDiv("完成后会自动贴出结果。如果它在等输入，点下面的按钮，或直接发消息。"),
		{
			"tag": "action",
			"actions": []map[string]any{
				btn("y", "primary", cardActInput, "y"),
				btn("n", "danger", cardActInput, "n"),
				btn("Enter", "default", cardActKey, "enter"),
				btn("Ctrl-C", "danger", cardActKey, "ctrl-c"),
				btn("查看画面", "default", cardActScreen, ""),
			},
		},
	}
	return cardJSON(fmt.Sprintf("命令运行中 · Job #%d", job.ID), "orange", els)
}

// watchCard 是 /watch 的刷新卡片：一块终端画面 + 停止按钮。
func watchCard(title, snapshot, key, recvType, recvID string, running bool) string {
	if strings.TrimSpace(snapshot) == "" {
		snapshot = "(画面为空)"
	}
	snapshot = headRunes(snapshot, cardSnapLimit)

	els := []map[string]any{
		cardDiv(fmt.Sprintf("**%s** · %s", title, time.Now().Format("15:04:05"))),
		cardPlain(snapshot),
	}
	if running {
		els = append(els, map[string]any{
			"tag": "action",
			"actions": []map[string]any{
				cardButton("停止刷新", "danger", map[string]any{
					"key": key, "rt": recvType, "rid": recvID, "act": cardActStop,
				}),
			},
		})
	}
	return cardJSON(title, "blue", els)
}

// stoppedCard 是刷新的最终态：保留最后一帧画面，去掉按钮。
func stoppedCard(title, note, snapshot string) string {
	if strings.TrimSpace(snapshot) == "" {
		snapshot = "(画面为空)"
	}
	els := []map[string]any{
		cardDiv("**" + title + "**"),
		cardPlain(headRunes(snapshot, cardSnapLimit)),
		cardDiv(note),
	}
	return cardJSON(title, "grey", els)
}

// helpCard 是人类用的上手指引：一屏说明 + 三个快捷按钮。
// AI bot 想要机器可读的版本，用 /help ai（纯文本）或 /help json。
func helpCard(key, recvType, recvID string) string {
	btn := func(label, act string) map[string]any {
		return cardButton(label, "default", map[string]any{
			"key": key, "rt": recvType, "rid": recvID, "act": act,
		})
	}
	els := []map[string]any{
		cardDiv("**直接执行**\n发一条命令即可（群里要 @ 本 bot）。\n例如 `df -h`、`cd /tmp && ls`"),
		cardDiv("**交互式**\n前台有进程在跑时，新消息就是它的 stdin。\n例如 `read x` 之后发 `y`；等输入超时后会发带按钮的卡片。"),
		cardDiv("**TUI / 实时画面**\n`/raw htop` 起程序 → `/screen` 看画面 → `/key ctrl-c` 退出。\n`/watch top --every 2s --for 5m` 让卡片自动刷新。"),
		cardDiv("**长任务与长输出**\n超过 8 秒先回执，结束后自动贴结果（最长跟踪 30 分钟）。\n超过 3000 字节的输出会作为 `.log` 文件发送。"),
		cardDiv("**会话**\n每个聊天一个持久 tmux 会话，bot 重启不丢；`/reset` 重置（会杀掉会话内进程）。"),
		{"tag": "hr"},
		cardDiv("`/screen [N]` 画面 · `/key <按键>` 按键 · `/raw <命令>` 前台运行\n" +
			"`/watch <命令>` 自动刷新 · `/watch stop` 停止 · `/jobs` 任务 · `/reset` 重置"),
		cardDiv("AI bot：发 `/help ai` 获取机器可读规格，`/help json` 获取 JSON 版。"),
		{
			"tag": "action",
			"actions": []map[string]any{
				btn("查看当前画面", cardActScreen),
				btn("任务状态", cardActJobs),
				btn("AI 规格", cardActAIHelp),
			},
		},
	}
	return cardJSON("终端 Bot 使用帮助", "blue", els)
}

// doneCard 把「运行中」的回执卡片改成结束态，避免过期按钮被继续点击。
func doneCard(job *Job, code int) string {
	status := "已完成。"
	template := "green"
	switch {
	case code > 0:
		status = fmt.Sprintf("已结束，退出码 %d。", code)
		template = "red"
	case code < 0:
		status = "已结束（未取到退出码）。"
		template = "grey"
	}
	els := []map[string]any{
		cardDiv(fmt.Sprintf("**Job #%d %s**", job.ID, status)),
		cardPlain(job.Cmd),
	}
	return cardJSON(fmt.Sprintf("命令已结束 · Job #%d", job.ID), template, els)
}

// ---- 发送与更新 ----

func sendCard(client *lark.Client, receiveID, receiveIDType, card string) (string, error) {
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(receiveIDType).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(receiveID).
			MsgType("interactive").
			Content(card).
			Build()).
		Build()
	resp, err := client.Im.Message.Create(context.Background(), req)
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", fmt.Errorf("发送卡片失败 code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.MessageId == nil {
		return "", fmt.Errorf("发送卡片成功但没有 message_id")
	}
	return *resp.Data.MessageId, nil
}

func patchCard(client *lark.Client, messageID, card string) error {
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().Content(card).Build()).
		Build()
	resp, err := client.Im.Message.Patch(context.Background(), req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("更新卡片失败 code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// ---- 卡片回调 ----

func toast(typ, content string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: typ, Content: content}}
}

// handleCardAction 处理按钮点击。
// 关键点：卡片回调里的操作者也必须过白名单——能看见卡片 != 有权按按钮。
func handleCardAction(client *lark.Client, mgr *SessionManager, cfg *Config, ev *callback.CardActionTriggerEvent) *callback.CardActionTriggerResponse {
	if ev == nil || ev.Event == nil || ev.Event.Action == nil || ev.Event.Operator == nil {
		return toast("error", "无效的卡片回调")
	}
	openID := ev.Event.Operator.OpenID
	userID := deref(ev.Event.Operator.UserID)
	if !cfg.Allowlist[openID] && !cfg.Allowlist[userID] {
		log.Printf("拒绝未授权的卡片操作 open_id=%q user_id=%q", openID, userID)
		return toast("error", "无权限")
	}

	v := ev.Event.Action.Value
	key, _ := v["key"].(string)
	act, _ := v["act"].(string)
	rt, _ := v["rt"].(string)
	rid, _ := v["rid"].(string)
	text, _ := v["text"].(string)

	// 这两个动作不需要会话：AI 规格只是发一段文本；停止刷新只动内存里的循环。
	switch act {
	case cardActAIHelp:
		go func() { reply(client, rid, rt, helpSpec(cfg)) }()
		return toast("success", "正在发送 AI 规格")
	case cardActStop:
		if stopWatch(key) {
			return toast("success", "已停止刷新")
		}
		return toast("info", "没有正在运行的 /watch")
	}

	sess := mgr.Lookup(key)
	if sess == nil {
		return toast("error", "会话已不存在（可能已 /reset）")
	}

	switch act {
	case cardActInput:
		if err := sess.SendInput(text); err != nil {
			return toast("error", "发送失败: "+err.Error())
		}
		return toast("success", "已发送: "+text)

	case cardActKey:
		if text == "" {
			text = "enter"
		}
		if err := sess.SendKey(text); err != nil {
			return toast("error", err.Error())
		}
		if text == "ctrl-c" {
			return toast("success", "已发送 Ctrl-C")
		}
		return toast("success", "已发送按键: "+text)

	case cardActScreen:
		go func() {
			out, err := sess.Snapshot(0)
			if err != nil {
				reply(client, rid, rt, "取画面失败: "+err.Error())
				return
			}
			if strings.TrimSpace(out) == "" {
				out = "(画面为空)"
			}
			reply(client, rid, rt, out)
		}()
		return toast("success", "正在发送画面")

	case cardActJobs:
		go func() { reply(client, rid, rt, jobStatusText(sess)) }()
		return toast("success", "正在发送任务状态")
	}
	return toast("info", "未知操作")
}

// ---- /watch：卡片原地刷新 ----

// watchEntry 每轮刷新一个实例。用指针比较来判断「map 里的是不是自己」，
// 避免上一轮退出时误删/误停用户刚启动的新一轮。
type watchEntry struct {
	cancel context.CancelFunc
	msgID  string
}

var (
	watchMu sync.Mutex
	watches = map[string]*watchEntry{}
)

func stopWatch(key string) bool {
	watchMu.Lock()
	e, ok := watches[key]
	if ok {
		delete(watches, key)
	}
	watchMu.Unlock()
	if ok {
		e.cancel()
	}
	return ok
}

func watchRunning(key string) bool {
	watchMu.Lock()
	defer watchMu.Unlock()
	_, ok := watches[key]
	return ok
}

// startWatch 发送一张会持续更新的卡片。
// 命令若为空，就只监视当前画面（适合已经在跑的进程）。
func startWatch(client *lark.Client, sess *Session, key, cmd string,
	every, duration time.Duration, receiveID, receiveIDType string) error {
	watchMu.Lock()
	if _, ok := watches[key]; ok {
		watchMu.Unlock()
		return fmt.Errorf("已有 /watch 在运行，先 /watch stop")
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &watchEntry{cancel: cancel}
	watches[key] = e
	watchMu.Unlock()

	title := "终端画面"
	if cmd != "" {
		title = cmd
		// 给 TUI 一点时间画第一帧。
		time.Sleep(500 * time.Millisecond)
	}
	initial, _ := sess.Snapshot(0)
	msgID, err := sendCard(client, receiveID, receiveIDType,
		watchCard(title, initial, key, receiveIDType, receiveID, true))
	if err != nil {
		watchMu.Lock()
		if watches[key] == e {
			delete(watches, key)
		}
		watchMu.Unlock()
		cancel()
		return err
	}
	e.msgID = msgID

	go watchLoop(ctx, e, client, sess, key, title, every, duration, receiveID, receiveIDType)
	return nil
}

func watchLoop(ctx context.Context, e *watchEntry, client *lark.Client, sess *Session,
	key, title string, every, duration time.Duration, receiveID, receiveIDType string) {
	defer func() {
		watchMu.Lock()
		if watches[key] == e {
			delete(watches, key)
		}
		watchMu.Unlock()
		e.cancel()
	}()

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	deadline := time.Now().Add(duration)
	idle := 0

	for {
		select {
		case <-ctx.Done():
			_ = patchCard(client, e.msgID,
				stoppedCard(title, "已停止刷新。若前台进程还在跑，用 /key ctrl-c 退出。", mustSnapshot(sess)))
			return

		case <-ticker.C:
			snap := mustSnapshot(sess)
			if time.Now().After(deadline) {
				_ = patchCard(client, e.msgID, stoppedCard(title,
					fmt.Sprintf("已停止：到达最长刷新时间 %s。若前台进程还在跑，用 /key ctrl-c 退出；需要的话重新 /watch。",
						duration.Round(time.Second)), snap))
				return
			}
			_ = patchCard(client, e.msgID,
				watchCard(title, snap, key, receiveIDType, receiveID, true))
			if sess.Busy() {
				idle = 0
			} else {
				idle++
			}
			if idle >= 2 {
				_ = patchCard(client, e.msgID,
					stoppedCard(title, "命令已结束。", snap))
				return
			}
		}
	}
}

func mustSnapshot(s *Session) string {
	out, _ := s.Snapshot(0)
	return out
}

// parseWatchArgs 解析 /watch 的参数。
//
//	/watch top --every 2s --for 5m
//	/watch stop
func parseWatchArgs(text string) (cmd string, every, duration time.Duration, stop bool, err error) {
	every = 2 * time.Second
	duration = 5 * time.Minute
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(text, "/watch")))
	if len(fields) > 0 && fields[0] == "stop" {
		return "", every, duration, true, nil
	}
	var rest []string
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "--every":
			if i+1 >= len(fields) {
				return "", every, duration, false, fmt.Errorf("--every 后面要跟时长，例如 2s")
			}
			i++
			d, e := time.ParseDuration(fields[i])
			if e != nil || d < 500*time.Millisecond {
				return "", every, duration, false, fmt.Errorf("--every 必须 >= 500ms")
			}
			every = d
		case "--for":
			if i+1 >= len(fields) {
				return "", every, duration, false, fmt.Errorf("--for 后面要跟时长，例如 5m")
			}
			i++
			d, e := time.ParseDuration(fields[i])
			if e != nil || d <= 0 {
				return "", every, duration, false, fmt.Errorf("--for 的时长无法解析")
			}
			duration = d
		default:
			rest = append(rest, fields[i])
		}
	}
	// 飞书对 PATCH 有频控，刷新间隔不小于 1 秒；单次刷新最长 30 分钟。
	if every < time.Second {
		every = time.Second
	}
	if duration > 30*time.Minute {
		duration = 30 * time.Minute
	}
	if duration < every {
		duration = every
	}
	return strings.Join(rest, " "), every, duration, false, nil
}
