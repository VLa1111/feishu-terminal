package main

import (
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func sp(s string) *string { return &s }

func mention(id string) *larkim.MentionEvent {
	return &larkim.MentionEvent{Id: &larkim.UserId{OpenId: sp(id)}}
}

// 群里可能同时有多个 bot。@ 了别人时本 bot 绝不能执行那条命令。
func TestMentionsBot(t *testing.T) {
	const self = "ou_self"
	const otherBot = "ou_other_bot"
	const human = "ou_human"

	cases := []struct {
		name string
		msg  *larkim.EventMessage
		want bool
	}{
		{"@了自己", &larkim.EventMessage{Mentions: []*larkim.MentionEvent{mention(self)}}, true},
		{"@了另一个 bot", &larkim.EventMessage{Mentions: []*larkim.MentionEvent{mention(otherBot)}}, false},
		{"@了人", &larkim.EventMessage{Mentions: []*larkim.MentionEvent{mention(human)}}, false},
		{"先@别人再@自己", &larkim.EventMessage{
			Mentions: []*larkim.MentionEvent{mention(otherBot), mention(self)}}, true},
		{"没有任何 @", &larkim.EventMessage{}, false},
		{"Mentions 为 nil", &larkim.EventMessage{Mentions: nil}, false},
	}
	for _, c := range cases {
		if got := mentionsBot(c.msg, self); got != c.want {
			t.Errorf("%s: mentionsBot = %v, want %v", c.name, got, c.want)
		}
	}
}

// 用 2026-10-16 真实闯祸的文本回归：AI agent 的正文里带了 @，
// 被 resolve_mentions 变成真 mention，整段思考/播报被当成命令执行。
func TestLooksLikeCommandRejectsRealIncident(t *testing.T) {
	bad := []string{
		// agent 的思考消息（多行 + emoji + 中文）
		"💭 Now I know the mechanism: `cc-connect send -s \"$CC_SESSION_KEY\" -m \"@_user_1 <命令>\"`.\n\nSo I should run:\n```\ncc-connect send ...\n```",
		// agent 的工具播报卡片
		"🔧 **工具 #8: Bash**\n---\n```bash\ncc-connect send -s \"$CC_SESSION_KEY\" -m \"@_user_1 nvidia-smi\"\n```",
		// agent 的普通回复正文
		"已下发到终端 bot：\n\n```\n@_user_1 nvidia-smi --query-gpu=memory.used --format=csv\n```\n结果稍后可见。",
		"",                                  // 空
		"pwd\nls\nwhoami",                   // 多行命令拼接，一条消息里塞多条指令
		"echo " + string(make([]byte, 400)), // 超长
	}
	for _, s := range bad {
		if looksLikeCommand(s) {
			t.Errorf("应当拒绝，却放行了: %.60q", s)
		}
	}

	good := []string{
		"pwd",
		"echo MENTION_OK && hostname && date",
		"nvidia-smi --query-gpu=index,name,memory.used,memory.total,utilization.gpu --format=csv",
		"cd /home/a/larkWorkspace && git status --short",
	}
	for _, s := range good {
		if !looksLikeCommand(s) {
			t.Errorf("应当放行，却被拒了: %q", s)
		}
	}
}

// 畸形事件不应该 panic。
func TestMentionsBotMalformed(t *testing.T) {
	msg := &larkim.EventMessage{Mentions: []*larkim.MentionEvent{
		nil,
		{},
		{Id: &larkim.UserId{}},
	}}
	if mentionsBot(msg, "ou_self") {
		t.Error("畸形 mention 不应判定为 @ 了自己")
	}
}
