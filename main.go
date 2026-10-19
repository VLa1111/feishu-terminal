package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

type msgContent struct {
	Text string `json:"text"`
}

var mentionRe = regexp.MustCompile(`@_user_\d+`)

// 本机器人自身 open_id 的缓存。首次需要时解析一次，之后直接读。
var (
	botOpenIDMu sync.Mutex
	botOpenID   string
)

// resolveBotOpenID 取得本机器人自己在这个应用视角下的 open_id。
// 群聊里必须靠它区分「@ 的是我」和「@ 的是群里另一个 bot」，不能只看文本里有没有 @。
// cfg.BotOpenID 是可选的手动覆盖；留空则调接口自动获取，失败时返回空串（调用方据此拒绝处理群消息）。
func resolveBotOpenID(ctx context.Context, client *lark.Client, cfg *Config) string {
	botOpenIDMu.Lock()
	defer botOpenIDMu.Unlock()
	if botOpenID != "" {
		return botOpenID
	}
	if cfg.BotOpenID != "" {
		botOpenID = cfg.BotOpenID
		return botOpenID
	}

	resp, err := client.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		log.Printf("获取自身 open_id 失败，群消息将不予响应: %v", err)
		return ""
	}
	var body struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(resp.RawBody, &body); err != nil || body.Code != 0 {
		log.Printf("解析自身信息失败，群消息将不予响应: err=%v code=%d msg=%s", err, body.Code, body.Msg)
		return ""
	}
	if body.Bot.OpenID == "" {
		log.Printf("自身 open_id 为空，群消息将不予响应")
		return ""
	}
	botOpenID = body.Bot.OpenID
	log.Printf("本机器人自身 open_id = %s", botOpenID)
	return botOpenID
}

// mentionsBot 判断这条消息是否 @ 了 openID 对应的成员。
func mentionsBot(msg *larkim.EventMessage, openID string) bool {
	for _, m := range msg.Mentions {
		if m == nil || m.Id == nil {
			continue
		}
		if deref(m.Id.OpenId) == openID {
			return true
		}
	}
	return false
}

// looksLikeCommand 判断来自 bot 的文本是否像一条 shell 命令。
// 判定刻意保守：一条命令应当是单行、不太长、纯 ASCII，且不带 markdown 痕迹。
// 这是为了挡住"AI 自然语言正文里恰好带了 @ 就被当命令执行"这类误触发，
// 不是安全边界——真正该在意的仍是白名单。
func looksLikeCommand(text string) bool {
	const maxLen = 300
	if text == "" || len(text) > maxLen {
		return false
	}
	if strings.ContainsAny(text, "\n\r") {
		return false
	}
	for _, r := range text {
		if r > unicode.MaxASCII {
			return false
		}
	}
	for _, bad := range []string{"```", "**", "~~", "<at ", "]("} {
		if strings.Contains(text, bad) {
			return false
		}
	}
	return true
}

// clip 截断文本用于日志，避免一条长消息把日志刷满。
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(共 " + strconv.Itoa(len(s)) + " 字节)"
}

func main() {
	// 升级自检会执行 `--version`：必须在加载配置、连飞书之前处理。
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-version", "version":
			fmt.Println(versionLine())
			return
		}
	}

	cfg := LoadConfig()
	if cfg.AppID == "" || cfg.AppSecret == "" {
		log.Fatalf("请设置 FEISHU_APP_ID 和 FEISHU_APP_SECRET（可写入 .env 文件）")
	}
	log.Printf("启动 %s", versionLine())

	client := lark.NewClient(cfg.AppID, cfg.AppSecret)
	zshDir, _ := filepath.Abs("zsh")
	jobDir := filepath.Join(os.TempDir(), "feishu-terminal", "jobs")
	mgr := NewSessionManager(cfg.Shell, zshDir, cfg.TmuxSocket, jobDir, cfg.Cols, cfg.Rows)

	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			go handleMessage(client, mgr, cfg, event)
			return nil
		}).
		// 卡片按钮回调。长连接模式下同样走这条 WebSocket，不需要回调地址。
		OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			return handleCardAction(client, mgr, cfg, event), nil
		})

	ws := larkws.NewClient(cfg.AppID, cfg.AppSecret,
		larkws.WithEventHandler(eventHandler),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
	)

	// 可选：后台定期检查新版本并通知最近活跃的聊天。
	if cfg.UpdateURL != "" && cfg.UpdateCheck > 0 {
		log.Printf("已启用新版本检查：每 %s 检查一次 %s", shortDur(cfg.UpdateCheck), cfg.UpdateURL)
		go updateCheckLoop(client, cfg)
	}

	log.Printf("启动飞书长连接客户端...")
	if err := ws.Start(context.Background()); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
}

func handleMessage(client *lark.Client, mgr *SessionManager, cfg *Config, event *larkim.P2MessageReceiveV1) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return
	}
	msg := event.Event.Message

	senderOpenID := ""
	senderUserID := ""
	senderType := ""
	if event.Event.Sender != nil {
		senderType = deref(event.Event.Sender.SenderType)
		if event.Event.Sender.SenderId != nil {
			senderOpenID = deref(event.Event.Sender.SenderId.OpenId)
			senderUserID = deref(event.Event.Sender.SenderId.UserId)
		}
	}

	chatType := deref(msg.ChatType)
	chatID := deref(msg.ChatId)

	text := ""
	if msg.Content != nil {
		var c msgContent
		_ = json.Unmarshal([]byte(*msg.Content), &c)
		text = c.Text
	}
	text = strings.TrimSpace(text)

	receiveID := senderOpenID
	receiveIDType := "open_id"
	key := senderOpenID
	if chatType == "group" {
		receiveID = chatID
		receiveIDType = "chat_id"
		key = chatID
		// 群聊里只响应【@ 了本机器人】的消息，再剥掉 @_user_N 得到真实命令。
		// 注意：不能只判断文本里有没有 "@_user_"——群里可能还有别的 bot，
		// 用户 @ 另一个 bot 时本 bot 同样会收到这条消息，那样会误执行给别人看的命令。
		self := resolveBotOpenID(context.Background(), client, cfg)
		if self == "" || !mentionsBot(msg, self) {
			return
		}
		text = mentionRe.ReplaceAllString(text, "")
		text = strings.TrimSpace(text)
	}
	if receiveID == "" || text == "" {
		return
	}

	if !cfg.Allowlist[senderOpenID] && !cfg.Allowlist[senderUserID] {
		log.Printf("拒绝未授权发送者 open_id=%q user_id=%q type=%q: %q（把对应 id 填入 FEISHU_ALLOW_OPEN_IDS 即可放行）", senderOpenID, senderUserID, senderType, text)
		return
	}
	// 记住最近活跃的聊天，供后台新版本检查主动推送。
	rememberChat(receiveID, receiveIDType)

	// 群聊里来自【其他 bot】的消息必须是"命令形状"才执行。
	// AI agent 输出的是自然语言，它只要在正文里带一个 @ 就会变成这里的一条命令；
	// 实测过一次：agent 的思考过程/工具播报里含 @，被当成命令整段执行了。
	// 只约束 bot，人类自己发的中文自然语言不受影响。
	if chatType == "group" && senderType == "bot" && !looksLikeCommand(text) {
		log.Printf("忽略非命令形状的 bot 消息 open_id=%q: %s", senderOpenID, clip(text, 160))
		reply(client, receiveID, receiveIDType,
			"已忽略：来自 bot 的消息不像单行命令（可能是 Agent 正文里带了 @）。只执行单行、不含中文/emoji、不超过 300 字符的命令。")
		return
	}

	switch {
	case text == "/help":
		if _, err := sendCard(client, receiveID, receiveIDType,
			helpCard(key, receiveIDType, receiveID)); err != nil {
			log.Printf("发送帮助卡片失败，退回文本: %v", err)
			reply(client, receiveID, receiveIDType, helpText())
		}
		return
	case text == "/help ai" || text == "/api":
		reply(client, receiveID, receiveIDType, helpSpec(cfg))
		return
	case text == "/help json":
		reply(client, receiveID, receiveIDType, helpSpecJSON(cfg))
		return
	case text == "/reset":
		stopWatch(key)
		mgr.Close(key)
		reply(client, receiveID, receiveIDType, "会话已重置")
		return

	// 版本与在线升级。这些命令不需要终端会话，放在 GetOrCreate 之前，
	// 这样即使建会话失败也能升级/排查。
	case text == "/version":
		reply(client, receiveID, receiveIDType, versionInfo(cfg))
		return
	case text == "/checkupdate" || strings.HasPrefix(text, "/checkupdate "):
		handleCheckUpdate(client, cfg, receiveID, receiveIDType,
			strings.TrimSpace(strings.TrimPrefix(text, "/checkupdate")))
		return
	case text == "/upgrade" || strings.HasPrefix(text, "/upgrade "):
		handleUpgradeCommand(client, cfg, receiveID, receiveIDType,
			strings.TrimSpace(strings.TrimPrefix(text, "/upgrade")))
		return
	case text == "/rollback":
		handleRollbackCommand(client, receiveID, receiveIDType)
		return
	}

	sess, err := mgr.GetOrCreate(key)
	if err != nil {
		reply(client, receiveID, receiveIDType, "启动 shell 失败: "+err.Error())
		return
	}

	if handleSlash(client, sess, key, text, receiveID, receiveIDType) {
		return
	}

	// 前台还有进程在跑：这条消息当作它的标准输入送进去，这是终端的语义。
	// 想跑新命令先 /key ctrl-c 中断，或等它结束。
	if sess.Busy() {
		if err := sess.SendInput(text); err != nil {
			reply(client, receiveID, receiveIDType, "发送输入失败: "+err.Error())
			return
		}
		info := sess.RunningJobInfo()
		if info == "" {
			info = "前台: " + sess.ForegroundCommand()
		}
		reply(client, receiveID, receiveIDType, "已发送到前台进程\n"+info)
		return
	}

	// docker attach 是交互式命令：普通模式下 stdout 被重定向到任务文件，
	// 容器画面在屏幕上看不到；而且脱离要用 Ctrl-P Ctrl-Q 而不是 Ctrl-C。
	if strings.HasPrefix(strings.TrimSpace(text), "docker attach") {
		reply(client, receiveID, receiveIDType,
			"⚠️ docker attach 建议用 /raw 启动：普通模式的输出会写进任务文件，屏幕上看不到。\n"+
				"已经跑起来的话，脱离用 /key ctrl-p 再 /key ctrl-q；不要用 Ctrl-C / Ctrl-D，那会把信号转给容器里的主进程。")
	}

	job, err := sess.Run(text)
	if errors.Is(err, ErrBusy) {
		reply(client, receiveID, receiveIDType, "有命令正在运行，请等它结束或 /key ctrl-c 中断。")
		return
	}
	if err != nil {
		reply(client, receiveID, receiveIDType, "执行失败: "+err.Error())
		return
	}

	if sess.Wait(job, cfg.ExecTimeout) {
		sendJobResult(client, cfg, sess, job, receiveID, receiveIDType)
		return
	}

	// 超时未结束：先给回执，完成后后台推送结果。
	// 注意这里不 kill 进程，它继续在 tmux 里跑。
	// 回执用交互卡片：等 y/n 的命令点一下按钮就能回答，不必打字。
	fg := sess.ForegroundCommand()
	if msgID, err := sendCard(client, receiveID, receiveIDType,
		promptCard(key, receiveIDType, receiveID, job, fg)); err != nil {
		log.Printf("发送交互卡片失败，退回文本: %v", err)
		if fg == "" {
			fg = "未知"
		}
		reply(client, receiveID, receiveIDType,
			fmt.Sprintf("▶ Job #%d 已启动（前台进程: %s）\n完成会自动贴出结果；期间发的消息会作为它的标准输入。\n/key ctrl-c 可中断，/screen 查看画面。", job.ID, fg))
	} else {
		job.SetCardID(msgID)
	}

	go func() {
		if sess.Wait(job, cfg.MaxJobDuration) {
			sendJobResult(client, cfg, sess, job, receiveID, receiveIDType)
			return
		}
		reply(client, receiveID, receiveIDType,
			fmt.Sprintf("Job #%d 超过 %s 仍未结束，已停止跟踪（进程还在跑）。用 /screen 查看，/key ctrl-c 中断。", job.ID, cfg.MaxJobDuration))
	}()
}

// handleSlash 处理与终端会话相关的斜杠命令；返回 true 表示已处理。
func handleSlash(client *lark.Client, sess *Session, key, text, receiveID, receiveIDType string) bool {
	if !strings.HasPrefix(text, "/") {
		return false
	}
	fields := strings.Fields(text)
	switch fields[0] {
	case "/screen":
		n := 0 // 0 = 当前可见的一屏，适合看 TUI
		if len(fields) > 1 {
			if v, err := strconv.Atoi(fields[1]); err == nil && v > 0 {
				n = v
			}
		}
		out, err := sess.Snapshot(n)
		if err != nil {
			reply(client, receiveID, receiveIDType, "取画面失败: "+err.Error())
			return true
		}
		if strings.TrimSpace(out) == "" {
			out = "(画面为空)"
		}
		reply(client, receiveID, receiveIDType, out)
		return true

	case "/key":
		if len(fields) < 2 {
			reply(client, receiveID, receiveIDType, "用法: /key ctrl-c（可选: tab / enter / esc / up / down / left / right / bspace / ctrl-d）")
			return true
		}
		if err := sess.SendKey(fields[1]); err != nil {
			reply(client, receiveID, receiveIDType, err.Error())
			return true
		}
		reply(client, receiveID, receiveIDType, "已发送按键: "+fields[1])
		return true

	case "/raw":
		cmd := strings.TrimSpace(strings.TrimPrefix(text, "/raw"))
		if cmd == "" {
			reply(client, receiveID, receiveIDType, "用法: /raw <命令>\n用于 top/htop/vim 这类必须独占终端的程序。之后用 /screen 看画面、/key 发按键、/key ctrl-c 退出。")
			return true
		}
		if err := sess.RunRaw(cmd); err != nil {
			reply(client, receiveID, receiveIDType, "启动失败: "+err.Error())
			return true
		}
		reply(client, receiveID, receiveIDType,
			"▶ 已在终端前台启动: "+cmd+"\n/screen 查看画面，/key 发按键，/key ctrl-c 退出。")
		return true

	case "/jobs":
		reply(client, receiveID, receiveIDType, jobStatusText(sess))
		return true

	case "/watch":
		cmd, every, duration, stop, err := parseWatchArgs(text)
		if err != nil {
			reply(client, receiveID, receiveIDType, err.Error()+
				"\n用法: /watch <命令> [--every 2s] [--for 5m]；/watch stop")
			return true
		}
		if stop {
			if stopWatch(key) {
				reply(client, receiveID, receiveIDType, "已停止刷新")
			} else {
				reply(client, receiveID, receiveIDType, "没有正在运行的 /watch")
			}
			return true
		}
		// 已有刷新在跑时先拒绝，免得命令起了却没有卡片显示。
		if watchRunning(key) {
			reply(client, receiveID, receiveIDType, "已有 /watch 在运行，先点卡片上的「停止刷新」或发 /watch stop。")
			return true
		}
		if cmd != "" {
			if err := sess.RunRaw(cmd); err != nil {
				if errors.Is(err, ErrBusy) {
					reply(client, receiveID, receiveIDType, "前台已有进程在跑。先 /key ctrl-c 中断，或直接 /watch 监视它。")
				} else {
					reply(client, receiveID, receiveIDType, "启动失败: "+err.Error())
				}
				return true
			}
		}
		if err := startWatch(client, sess, key, cmd, every, duration, receiveID, receiveIDType); err != nil {
			msg := "启动刷新卡片失败: " + err.Error()
			if cmd != "" {
				msg += "\n注意：命令已经在前台跑起来了，用 /screen 查看，/key ctrl-c 退出。"
			}
			reply(client, receiveID, receiveIDType, msg)
		}
		return true
	}

	// 未知的 /单词：给提示，而不是丢给 shell 去报错。
	// 绝对路径（/usr/bin/foo）后半段带斜杠，不会被拦。
	if len(fields[0]) > 1 && !strings.Contains(fields[0][1:], "/") {
		reply(client, receiveID, receiveIDType,
			"未知命令: "+fields[0]+"\n发 /help 查看用法；AI bot 发 /help ai 获取能力规格。")
		return true
	}
	return false
}

// jobStatusText 汇总当前任务/前台进程，供 /jobs 和卡片按钮共用。
func jobStatusText(sess *Session) string {
	if info := sess.RunningJobInfo(); info != "" {
		return info
	}
	if fg := sess.ForegroundCommand(); fg != "" && sess.Busy() {
		return "前台进程: " + fg + "（/raw 模式，没有跟踪任务）"
	}
	return "当前没有正在跟踪的任务"
}

func sendJobResult(client *lark.Client, cfg *Config, sess *Session, job *Job, receiveID, receiveIDType string) {
	out, code := sess.ReadResult(job)
	if strings.TrimSpace(out) == "" {
		out = "(无输出)"
	}

	// 命令结束了，把回执卡片上的按钮撤掉，免得之后误点（例如在提示符下点到 y）。
	if id := job.CardID(); id != "" {
		go func() {
			if err := patchCard(client, id, doneCard(job, code)); err != nil {
				log.Printf("更新回执卡片失败: %v", err)
			}
		}()
	}

	full := "$ " + job.Cmd + "\n" + out
	if code > 0 {
		full += fmt.Sprintf("\n[退出码 %d]", code)
	}
	if p := sess.CurrentPrompt(); p != "" {
		full += "\n" + p
	}

	if len(full) <= cfg.OutputLimit {
		reply(client, receiveID, receiveIDType, full)
		return
	}

	// 超长：完整输出上传成文件，正文只留首尾，避免刷屏也避免丢信息。
	if fileKey, err := uploadLog(client, job.Cmd, out); err == nil {
		summary := "$ " + job.Cmd + "\n" +
			headRunes(out, 1500) +
			fmt.Sprintf("\n…（共 %d 字节，完整输出见附件）\n", len(out)) +
			tailRunes(out, 800)
		if code > 0 {
			summary += fmt.Sprintf("\n[退出码 %d]", code)
		}
		reply(client, receiveID, receiveIDType, summary)
		sendFileMsg(client, receiveID, receiveIDType, fileKey)
		return
	} else {
		log.Printf("上传日志文件失败，改为分段发送: %v", err)
	}

	for _, part := range splitRunes(full, cfg.OutputLimit) {
		reply(client, receiveID, receiveIDType, part)
	}
}

// uploadLog 把完整输出上传为飞书文件，返回 file_key。
// 需要应用具备上传文件资源的权限；失败时调用方退化为分段发送。
func uploadLog(client *lark.Client, cmd, out string) (string, error) {
	f, err := os.CreateTemp("", "feishu-term-*.log")
	if err != nil {
		return "", err
	}
	name := "output-" + time.Now().Format("20060102-150405") + ".log"
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	if _, err := f.WriteString("$ " + cmd + "\n" + out); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return "", err
	}

	req := larkim.NewCreateFileReqBuilder().
		Body(larkim.NewCreateFileReqBodyBuilder().
			FileType("stream").
			FileName(name).
			File(f).
			Build()).
		Build()
	resp, err := client.Im.File.Create(context.Background(), req)
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", fmt.Errorf("上传文件失败 code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.FileKey == nil {
		return "", fmt.Errorf("上传文件成功但未返回 file_key")
	}
	return *resp.Data.FileKey, nil
}

func sendFileMsg(client *lark.Client, receiveID, receiveIDType, fileKey string) {
	content, _ := json.Marshal(map[string]string{"file_key": fileKey})
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(receiveIDType).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(receiveID).
			MsgType("file").
			Content(string(content)).
			Build()).
		Build()
	resp, err := client.Im.Message.Create(context.Background(), req)
	if err != nil {
		log.Printf("发送文件消息失败: %v", err)
		return
	}
	if !resp.Success() {
		log.Printf("发送文件消息失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
}

// headRunes / tailRunes / splitRunes 都按 rune 切分，避免把 UTF-8 汉字截成半个。
func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func splitRunes(s string, n int) []string {
	r := []rune(s)
	var out []string
	for len(r) > 0 {
		k := n
		if len(r) < k {
			k = len(r)
		}
		out = append(out, string(r[:k]))
		r = r[k:]
	}
	return out
}

func reply(client *lark.Client, receiveID, receiveIDType, text string) {
	const maxLen = 20000
	if len(text) > maxLen {
		text = headRunes(text, maxLen) + "\n...(已截断)"
	}
	content, _ := json.Marshal(map[string]string{"text": text})

	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(receiveIDType).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(receiveID).
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()

	resp, err := client.Im.Message.Create(context.Background(), req)
	if err != nil {
		log.Printf("回复消息失败: %v", err)
		return
	}
	if !resp.Success() {
		log.Printf("回复消息失败: code=%d msg=%s", resp.Code, resp.Msg)
	}
}

func helpText() string {
	return "直接发送内容即可作为 shell 命令执行。\n" +
		"⚠️ 前台有进程在跑时，新消息会作为它的标准输入（回答 y/n 等）。\n" +
		"\n" +
		"/help          查看本帮助（卡片）\n" +
		"/help ai       AI bot 用的能力规格（纯文本）\n" +
		"/help json     同上，JSON 格式\n" +
		"/reset         重置当前终端会话（会杀掉会话内所有进程）\n" +
		"/screen [行数]  查看终端当前画面（默认当前一屏）\n" +
		"/key <按键>    发送按键: ctrl-c / ctrl-p / ctrl-q / tab / enter / esc / up / down / left / right / bspace\n" +
		"/raw <命令>    在前台直接运行 TUI 或交互命令（top/htop/vim/docker attach），配合 /screen 与 /key\n" +
		"/watch <命令> [--every 2s] [--for 5m]\n" +
		"               在卡片里持续刷新终端画面（不给命令就监视当前前台）\n" +
		"/watch stop    停止刷新\n" +
		"/jobs          查看正在跟踪的任务\n" +
		"/version       查看版本、运行时长、可执行文件与更新源\n" +
		"/checkupdate [清单URL]  检查新版本\n" +
		"/upgrade [清单URL] [sha256] [--force]\n" +
		"               下载新版本并原地重启（tmux 会话不受影响）\n" +
		"/rollback      回滚到上一个版本（可反复切换）\n" +
		"\n" +
		"长命令会自动转后台：先回执（卡片上带 y / n / Enter / Ctrl-C 按钮），结束后推送结果；超长输出以文件形式发送。"
}

// shortDur 让配置里的时长在帮助文本里好看一点：30m0s → 30m，8s → 8s。
func shortDur(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	if d >= time.Second && d%time.Second == 0 {
		return strconv.Itoa(int(d/time.Second)) + "s"
	}
	return d.String()
}

// helpSpec 是给 AI agent 读的能力规格：一屏内说清语法、状态机、限制和坑。
// 刻意用纯文本而不是卡片——agent 读群消息时卡片 JSON 噪音太大。
func helpSpec(cfg *Config) string {
	return "feishu-terminal v2 能力规格（供 AI agent 读取）\n" +
		"版本: " + version + "（升级相关命令见文末）\n" +
		"用法：群里 @本 bot + 内容；私聊直接发。一条消息 = 一行，不要用 markdown。\n" +
		"\n" +
		"## 基本\n" +
		"- 直接发 shell 命令，如 `df -h`；输出作为后续群消息返回。\n" +
		"- 会话持久：每个聊天一个 tmux shell，cd/环境变量/后台进程跨消息保留，bot 重启不丢。\n" +
		"\n" +
		"## 状态机（最关键）\n" +
		"- 空闲时：消息 = 新命令。\n" +
		"- 前台有进程时：消息 = 该进程的 stdin，不执行。要跑新命令先 /key ctrl-c。\n" +
		"- 命令 " + shortDur(cfg.ExecTimeout) + " 未结束：先回「运行中」卡片，结束后自动推送结果（最长跟踪 " + shortDur(cfg.MaxJobDuration) + "）。\n" +
		"- 输出 >" + strconv.Itoa(cfg.OutputLimit) + " 字节：自动上传 .log，正文只留首尾。\n" +
		"\n" +
		"## 命令\n" +
		"/help            人类帮助卡片\n" +
		"/help ai         本规格\n" +
		"/help json       本规格的 JSON 版\n" +
		"/screen [N]      终端当前画面；N 省略 = 当前一屏\n" +
		"/key <KEY>       发按键: ctrl-c|ctrl-p|ctrl-q|tab|enter|esc|up|down|left|right|bspace\n" +
		"/raw <CMD>       前台运行交互/TUI 程序，输出直接进终端画面\n" +
		"/watch <CMD> [--every 2s] [--for 5m]   卡片原地刷新；省略 CMD = 监视当前前台\n" +
		"/watch stop      停止刷新（不杀进程）\n" +
		"/jobs            当前跟踪的任务\n" +
		"/reset           重置会话（杀掉会话内所有进程）\n" +
		"/version         版本 / 运行时长 / 可执行文件路径\n" +
		"/checkupdate [URL]  检查 release.json 是否有新版本（省略 URL 用 FEISHU_UPDATE_URL）\n" +
		"/upgrade [URL] [sha256] [--force]  下载并原地重启；旧版本留在 .rollback\n" +
		"/rollback        回滚到上一个版本（再执行一次可切回）\n" +
		"\n" +
		"## 交互范式\n" +
		"- y/n 确认：直接发 `y`，或点卡片按钮。\n" +
		"- TUI：/raw htop → /screen 或 /watch → /key ctrl-c。\n" +
		"- docker attach：必须 /raw docker attach <id>；脱离 = /key ctrl-p 然后 /key ctrl-q。不要用 Ctrl-C/Ctrl-D。\n" +
		"- 密码提示（sudo/ssh/passwd）读 /dev/tty，无法通过聊天输入，也不要发。\n" +
		"\n" +
		"## 给 agent 的约束\n" +
		"- 每次只发一条命令；需要多条就分多次。\n" +
		"- 收到「已启动」后不要轮询，结果会作为后续群消息出现。\n" +
		"- 不确定终端状态时先 /screen。\n" +
		"- 交互式命令发出后，本轮拿不到结果；等下一轮从群历史里读。"
}

// helpSpecJSON 与 helpSpec 内容一致，给需要结构化解析的 agent。
func helpSpecJSON(cfg *Config) string {
	spec := map[string]any{
		"name":         "feishu-terminal",
		"version":      version,
		"spec_version": 2,
		"transport":    "Feishu bot; group messages must mention the bot, direct messages need no mention",
		"input":        "one line per message, plain text, no markdown",
		"state_machine": []string{
			"idle: message = new shell command",
			"foreground process running: message = stdin of that process (NOT executed)",
			"command still running after " + shortDur(cfg.ExecTimeout) + ": 'running' card sent; result pushed later",
			"max job tracking: " + shortDur(cfg.MaxJobDuration),
			"output > " + strconv.Itoa(cfg.OutputLimit) + " bytes: uploaded as .log file; message keeps head/tail",
		},
		"commands": []map[string]string{
			{"/help": "human help card"},
			{"/help ai": "this spec as text"},
			{"/help json": "this spec as JSON"},
			{"/screen [N]": "capture terminal; N omitted = current viewport"},
			{"/key <KEY>": "send key: ctrl-c|ctrl-p|ctrl-q|tab|enter|esc|up|down|left|right|bspace"},
			{"/raw <CMD>": "run interactive/TUI program in the foreground"},
			{"/watch <CMD> [--every 2s] [--for 5m]": "auto-refreshing card; omit CMD to watch the current foreground"},
			{"/watch stop": "stop refreshing (does not kill the process)"},
			{"/jobs": "current job/foreground status"},
			{"/reset": "kill the session and all its processes"},
			{"/version": "version, uptime, executable path, update source"},
			{"/checkupdate [URL]": "check release.json for a newer version"},
			{"/upgrade [URL] [sha256] [--force]": "download, verify, self-test, atomically replace and re-exec; tmux sessions survive"},
			{"/rollback": "switch back to the previous binary (run again to switch forward)"},
		},
		"patterns": map[string]string{
			"y/n prompt":      "send 'y' or tap the card button",
			"TUI":             "/raw htop -> /screen or /watch -> /key ctrl-c",
			"docker attach":   "/raw docker attach <id>; detach with /key ctrl-p then /key ctrl-q; never Ctrl-C/Ctrl-D",
			"password prompt": "unsupported (reads /dev/tty); do not send passwords",
		},
		"agent_rules": []string{
			"send exactly one command per message",
			"after 'started', do not poll: the result appears as a later group message",
			"when unsure about terminal state, send /screen first",
			"interactive commands return no value in the same turn",
		},
	}
	b, _ := json.MarshalIndent(spec, "", "  ")
	return string(b)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
