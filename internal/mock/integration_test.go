// 端到端联调测试。
//
// 这里刻意不去 mock bot.Engine 或 onebot.Client——被验证的正是它们。
// mock 只替换了两端：OneBot 协议端（NapCat 的位置）与模型服务（DeepSeek 的位置）。
// 所以这些测试跑通，等于「消息进来 → 判断 → 组上下文 → 调模型 → 分段发出」整条链路跑通。
package mock_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vex/qqbot/internal/bot"
	"github.com/vex/qqbot/internal/llm"
	"github.com/vex/qqbot/internal/mock"
	"github.com/vex/qqbot/internal/onebot"
	"github.com/vex/qqbot/internal/store"
)

const (
	testSelfID = int64(10001)
	testGroup  = int64(30001)
	testUser   = int64(20001)
)

// harness 组装一套完整的运行时：真 store + 真引擎 + 真协议客户端 + 假两端。
type harness struct {
	t      *testing.T
	ob     *mock.OneBotServer
	llm    *mock.LLMServer
	st     *store.Store
	engine *bot.Engine
	client *onebot.Client
	cancel context.CancelFunc
}

func newHarness(t *testing.T, tweak func(*store.Settings) error) *harness {
	t.Helper()

	ob, err := mock.NewOneBot(mock.OneBotOptions{
		SelfID:   testSelfID,
		Nickname: "小助手",
		Groups: []mock.Group{
			{GroupID: testGroup, GroupName: "测试群", MemberCount: 3},
			{GroupID: 30002, GroupName: "另一个群", MemberCount: 5},
		},
	})
	if err != nil {
		t.Fatalf("启动 mock 协议端失败: %v", err)
	}

	ls, err := mock.NewLLM(mock.LLMOptions{APIKey: "sk-mock"})
	if err != nil {
		_ = ob.Close()
		t.Fatalf("启动 mock 模型服务失败: %v", err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "qqbot.db"))
	if err != nil {
		_ = ob.Close()
		_ = ls.Close()
		t.Fatalf("打开数据库失败: %v", err)
	}

	_, err = st.UpdateSettings(func(v *store.Settings) error {
		v.LLM.BaseURL = ls.URL()
		v.LLM.APIKey = "sk-mock"
		v.LLM.Model = "deepseek-flash"
		v.LLM.Thinking = false
		v.LLM.TimeoutSec = 10
		// 默认关掉被动接话，让每个用例只验证它自己关心的触发路径
		v.Bot.PassiveRate = 0
		v.Bot.WakeWords = []string{"小助手"}
		if tweak != nil {
			return tweak(v)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("写入设置失败: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	engine := bot.New(st, nil, logger)
	client := onebot.New(ob.URL(), "", logger, nil)
	engine.SetClient(client)
	engine.SetSelf(testSelfID, "小助手")
	client.SetHandler(engine.HandleEvent)

	ctx, cancel := context.WithCancel(context.Background())
	go client.Run(ctx)

	h := &harness{t: t, ob: ob, llm: ls, st: st, engine: engine, client: client, cancel: cancel}
	t.Cleanup(h.close)

	h.waitConnected(5 * time.Second)
	return h
}

func (h *harness) close() {
	h.cancel()
	_ = h.ob.Close()
	_ = h.llm.Close()
	// 给在途的 reply goroutine 一点收尾时间，避免关闭数据库时的噪音日志
	time.Sleep(100 * time.Millisecond)
	_ = h.st.Close()
}

func (h *harness) waitConnected(d time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if h.client.Status().Connected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("协议端在 %s 内未连接", d)
}

// waitSent 等到机器人发出至少 n 条消息。超时则返回当前已有的并让调用方断言。
func (h *harness) waitSent(n int, d time.Duration) []mock.Sent {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	sent, err := h.ob.WaitSent(ctx, n)
	if err != nil {
		h.t.Fatalf("等待 %d 条发送超时，实际 %d 条: %v", n, len(sent), err)
	}
	return sent
}

// expectSentCount 断言 d 时间后发送总条数稳定在 want。
// 注意是「总条数」而不是「新增条数」：避免调用方忘了减掉已发出的基线。
func (h *harness) expectSentCount(want int, d time.Duration) {
	h.t.Helper()
	time.Sleep(d)
	if got := len(h.ob.Sent()); got != want {
		h.t.Fatalf("期望发送 %d 条，实际 %d 条: %+v", want, got, h.ob.Sent())
	}
}

func (h *harness) inject(m mock.InjectMessage) int64 {
	h.t.Helper()
	id, err := h.ob.InjectMessage(m)
	if err != nil {
		h.t.Fatalf("注入事件失败: %v", err)
	}
	return id
}

func (h *harness) lastRequest() mock.LLMRequest {
	h.t.Helper()
	reqs := h.llm.Requests()
	if len(reqs) == 0 {
		h.t.Fatal("模型一次都没被调用")
	}
	return reqs[len(reqs)-1]
}

// --- 主链路 ---

func TestGroupMentionReply(t *testing.T) {
	h := newHarness(t, nil)
	h.llm.SetReply("好的，我记住了。")

	h.inject(mock.InjectMessage{
		GroupID: testGroup, UserID: testUser, Text: "你好", AtSelf: true,
	})

	sent := h.waitSent(1, 5*time.Second)
	if len(sent) != 1 {
		t.Fatalf("期望发出 1 条，实际 %d 条", len(sent))
	}
	m := sent[0]
	if m.Action != "send_group_msg" {
		t.Errorf("动作应为 send_group_msg，实际 %q", m.Action)
	}
	if m.GroupID != testGroup {
		t.Errorf("群号应为 %d，实际 %d", testGroup, m.GroupID)
	}
	if m.Text != "好的，我记住了。" {
		t.Errorf("文本不符，实际 %q", m.Text)
	}
	if m.ReplyTo == 0 {
		t.Error("群消息首段应带引用回复段")
	}
	if h.llm.Calls() != 1 {
		t.Errorf("期望调用模型 1 次，实际 %d 次", h.llm.Calls())
	}
}

func TestPrivateMessage(t *testing.T) {
	h := newHarness(t, nil)
	h.llm.SetReply("私聊也行。")

	h.inject(mock.InjectMessage{UserID: testUser, Text: "在吗", Private: true})

	sent := h.waitSent(1, 5*time.Second)
	if sent[0].Action != "send_private_msg" {
		t.Errorf("动作应为 send_private_msg，实际 %q", sent[0].Action)
	}
	if sent[0].UserID != testUser {
		t.Errorf("目标用户应为 %d，实际 %d", testUser, sent[0].UserID)
	}
	// 私聊不引用、不 @，直接发正文
	if sent[0].ReplyTo != 0 || sent[0].AtUser != 0 {
		t.Error("私聊首段不应带引用或 @")
	}
}

// TestMentionWithoutText 验证「只 @ 不说话」不去烧 token。
func TestMentionWithoutText(t *testing.T) {
	h := newHarness(t, nil)

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, AtSelf: true})

	sent := h.waitSent(1, 5*time.Second)
	if !strings.Contains(sent[0].Text, "在的") {
		t.Errorf("应回一句简短回应，实际 %q", sent[0].Text)
	}
	if h.llm.Calls() != 0 {
		t.Errorf("仅 @ 不应调用模型，实际调用 %d 次", h.llm.Calls())
	}
}

// --- 请求体正确性 ---

func TestThinkingDisabledByDefault(t *testing.T) {
	h := newHarness(t, nil)

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "测试", AtSelf: true})
	h.waitSent(1, 5*time.Second)

	req := h.lastRequest()
	if req.ThinkingType() != "disabled" {
		t.Errorf("思考模式应显式关闭，实际 thinking.type=%q", req.ThinkingType())
	}
	if req.Temperature == nil {
		t.Error("非思考模式应发送 temperature")
	}
	if req.Model != "deepseek-flash" {
		t.Errorf("模型名应为 deepseek-flash，实际 %q", req.Model)
	}
}

func TestThinkingEnabledDropsTemperature(t *testing.T) {
	h := newHarness(t, func(v *store.Settings) error {
		v.LLM.Thinking = true
		return nil
	})

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "测试", AtSelf: true})
	h.waitSent(1, 5*time.Second)

	req := h.lastRequest()
	if req.ThinkingType() != "enabled" {
		t.Errorf("应开启思考模式，实际 thinking.type=%q", req.ThinkingType())
	}
	if req.Temperature != nil {
		t.Error("思考模式下不应发送 temperature（会被忽略，发了也是浪费）")
	}
}

func TestSystemPromptHasPersonaAndRules(t *testing.T) {
	h := newHarness(t, nil)

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "测试", AtSelf: true})
	h.waitSent(1, 5*time.Second)

	sys := h.lastRequest().SystemText()
	if !strings.Contains(sys, "helpful assistant") {
		t.Errorf("系统提示应包含默认人格，实际:\n%s", sys)
	}
	if !strings.Contains(sys, "Markdown") {
		t.Errorf("系统提示应包含格式约束（禁 Markdown），实际:\n%s", sys)
	}
	if !strings.Contains(sys, "测试群") {
		t.Errorf("系统提示应带上群名，实际:\n%s", sys)
	}
}

// --- 触发判定 ---

func TestDedupeSameMessageID(t *testing.T) {
	h := newHarness(t, nil)

	// 换一个用户来重投同一条消息：这样即使去重失效，
	// 用户级冷却也不会把第二次拦下来，能真正测到去重逻辑。
	id := h.inject(mock.InjectMessage{
		GroupID: testGroup, UserID: testUser, Text: "重复消息", AtSelf: true,
	})
	h.waitSent(1, 5*time.Second)

	h.inject(mock.InjectMessage{
		GroupID: testGroup, UserID: testUser + 1, Text: "重复消息", AtSelf: true, MessageID: id,
	})

	h.expectSentCount(1, 600*time.Millisecond)
	if got := h.llm.Calls(); got != 1 {
		t.Errorf("重复消息应被去重，模型只该调用 1 次，实际 %d 次", got)
	}
}

func TestUserCooldown(t *testing.T) {
	h := newHarness(t, nil)

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "第一次", AtSelf: true})
	h.waitSent(1, 5*time.Second)

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "第二次", AtSelf: true})

	h.expectSentCount(1, 600*time.Millisecond)
	if got := h.llm.Calls(); got != 1 {
		t.Errorf("3 秒内同一用户连发应被冷却拦截，模型调用 %d 次", got)
	}
}

func TestPassiveRateZeroIgnoresPlainMessage(t *testing.T) {
	h := newHarness(t, func(v *store.Settings) error {
		v.Bot.PassiveRate = 0
		return nil
	})

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "今天天气不错"})

	h.expectSentCount(0, 500*time.Millisecond)
	if got := h.llm.Calls(); got != 0 {
		t.Errorf("被动接话关闭时不应调用模型，实际 %d 次", got)
	}
}

func TestPassiveRateFullAlwaysReplies(t *testing.T) {
	h := newHarness(t, func(v *store.Settings) error {
		v.Bot.PassiveRate = 1.0
		v.Bot.GroupCooldownSec = 0
		return nil
	})
	h.llm.SetReply("我插一句。")

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "今天天气不错"})
	h.waitSent(1, 5*time.Second)

	// 被动接话不该 @ 任何人，否则像在抢话
	if sent := h.ob.Sent(); sent[0].AtUser != 0 {
		t.Error("被动接话不应 @ 用户")
	}
}

func TestWakeWordTriggers(t *testing.T) {
	h := newHarness(t, nil)
	h.llm.SetReply("天气还行。")

	h.inject(mock.InjectMessage{
		GroupID: testGroup, UserID: testUser, Text: "小助手 今天天气怎么样",
	})
	h.waitSent(1, 5*time.Second)

	// 唤醒词必须被剥掉，否则模型会把它当成用户正文的一部分
	got := h.lastRequest().LastUserText()
	if strings.Contains(got, "小助手") {
		t.Errorf("唤醒词应被剥离，模型收到的正文是 %q", got)
	}
	if got != "今天天气怎么样" {
		t.Errorf("正文应为「今天天气怎么样」，实际 %q", got)
	}
}

func TestBlacklistUser(t *testing.T) {
	h := newHarness(t, func(v *store.Settings) error {
		v.Bot.BlacklistUsers = []int64{testUser}
		return nil
	})

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "嗨", AtSelf: true})

	h.expectSentCount(0, 500*time.Millisecond)
	if got := h.llm.Calls(); got != 0 {
		t.Errorf("黑名单用户不应触发模型调用，实际 %d 次", got)
	}
}

func TestWhitelistGroup(t *testing.T) {
	h := newHarness(t, func(v *store.Settings) error {
		v.Bot.WhitelistGroups = []int64{30002}
		return nil
	})

	// 30001 不在白名单
	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "嗨", AtSelf: true})
	h.expectSentCount(0, 500*time.Millisecond)

	// 30002 在白名单
	h.inject(mock.InjectMessage{GroupID: 30002, UserID: testUser + 5, Text: "嗨", AtSelf: true})
	h.waitSent(1, 5*time.Second)
}

func TestIgnoresSelfMessage(t *testing.T) {
	h := newHarness(t, nil)

	h.inject(mock.InjectMessage{
		GroupID: testGroup, UserID: testSelfID, Text: "我自己说的话", AtSelf: true,
	})

	h.expectSentCount(0, 500*time.Millisecond)
	if got := h.llm.Calls(); got != 0 {
		t.Errorf("不应处理自己发的消息，模型调用 %d 次", got)
	}
}

// --- 投递 ---

func TestMultiSegmentDelivery(t *testing.T) {
	h := newHarness(t, nil)

	// 每句 115 字，三句合计超过单段 120 字上限，必然切成 3 段
	sentence := strings.Repeat("分段投递测试", 19) + "。"
	text := sentence + sentence + sentence
	h.llm.SetReply(text)

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "说长一点", AtSelf: true})

	sent := h.waitSent(3, 10*time.Second)
	if len(sent) != 3 {
		t.Fatalf("期望切成 3 段，实际 %d 段", len(sent))
	}
	if sent[0].ReplyTo == 0 {
		t.Error("首段应带引用")
	}
	for i := 1; i < len(sent); i++ {
		if sent[i].ReplyTo != 0 {
			t.Errorf("第 %d 段不应带引用，实际引用了 %d", i, sent[i].ReplyTo)
		}
		if sent[i].AtUser != 0 {
			t.Errorf("第 %d 段不应 @ 用户", i)
		}
	}

	// 拼回来必须等于原文，不能丢字
	var sb strings.Builder
	for _, m := range sent {
		sb.WriteString(m.Text)
	}
	if sb.String() != text {
		t.Errorf("分段拼接后与原文不一致\n原文: %q\n拼接: %q", text, sb.String())
	}
}

func TestMarkdownStripped(t *testing.T) {
	h := newHarness(t, nil)
	h.llm.SetReply("**重点**：`代码` 和 ## 标题")

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "测试", AtSelf: true})
	h.waitSent(1, 5*time.Second)

	got := h.ob.Sent()[0].Text
	for _, bad := range []string{"**", "`", "##"} {
		if strings.Contains(got, bad) {
			t.Errorf("输出不应残留 Markdown 记号 %q，实际 %q", bad, got)
		}
	}
}

// --- 故障路径 ---

func TestLLMFailureSendsFallback(t *testing.T) {
	h := newHarness(t, nil)
	// 客户端最多尝试 3 次，全部失败才会走兜底
	h.llm.FailNext(3)

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "在吗", AtSelf: true})

	sent := h.waitSent(1, 12*time.Second)
	if !strings.Contains(sent[0].Text, "卡住") {
		t.Errorf("模型失败时应给兜底回复，实际 %q", sent[0].Text)
	}
	if got := h.llm.Calls(); got != 3 {
		t.Errorf("应重试到 3 次上限，实际调用 %d 次", got)
	}
}

func TestLLMRecoversAfterTransientFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.llm.SetReply("恢复了。")
	// 只失败一次，第二次重试应成功
	h.llm.FailNext(1)

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "在吗", AtSelf: true})

	sent := h.waitSent(1, 10*time.Second)
	if sent[0].Text != "恢复了。" {
		t.Errorf("重试成功后应返回正常内容，实际 %q", sent[0].Text)
	}
	if got := h.llm.Calls(); got != 2 {
		t.Errorf("期望 1 次失败 + 1 次成功，实际调用 %d 次", got)
	}
}

func TestSendFailureRecorded(t *testing.T) {
	h := newHarness(t, nil)
	h.ob.SetRejectSend(true)

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "在吗", AtSelf: true})

	// 发送失败不该让进程崩，也不该重试到刷屏
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if h.engine.Stats().Errors > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("发送失败应计入错误统计，实际 %+v", h.engine.Stats())
}

// --- 连接与上下文 ---

func TestReconnectAfterDrop(t *testing.T) {
	h := newHarness(t, nil)
	h.llm.SetReply("重连成功。")

	h.ob.DropPeers()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s := h.client.Status()
		if s.Connected && s.Reconnects >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s := h.client.Status()
	if !s.Connected || s.Reconnects < 1 {
		t.Fatalf("断线后未重连，状态: %+v", s)
	}

	// 重连后链路要能继续工作
	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser + 9, Text: "还在吗", AtSelf: true})
	h.waitSent(1, 5*time.Second)
}

func TestContextAccumulatesAcrossTurns(t *testing.T) {
	h := newHarness(t, nil)

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "我叫小明", AtSelf: true})
	h.waitSent(1, 5*time.Second)

	// 换个人说话绕过用户级冷却，但仍在同一个群会话里
	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser + 2, Text: "我叫什么", AtSelf: true})
	h.waitSent(2, 5*time.Second)

	second := h.lastRequest()
	if len(second.Messages) < 4 {
		t.Fatalf("第二次请求应带历史，实际只有 %d 条消息", len(second.Messages))
	}

	found := false
	for _, m := range second.Messages {
		if m.Role == "user" && m.Content == "我叫小明" {
			found = true
		}
	}
	if !found {
		t.Errorf("上下文未累积第一轮用户消息，实际: %+v", second.Messages)
	}
	// 历史必须在当前这句之前
	if second.LastUserText() != "我叫什么" {
		t.Errorf("最后一条 user 应是当前消息，实际 %q", second.LastUserText())
	}
}

func TestUsageRecorded(t *testing.T) {
	h := newHarness(t, nil)

	h.inject(mock.InjectMessage{GroupID: testGroup, UserID: testUser, Text: "记一下用量", AtSelf: true})
	h.waitSent(1, 5*time.Second)

	day := time.Now().Format("2006-01-02")
	// 给落库一点时间
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sum, err := h.st.UsageForDay(day)
		if err != nil {
			t.Fatalf("查询用量失败: %v", err)
		}
		if sum.Requests >= 1 {
			if sum.PromptTokens <= 0 {
				t.Errorf("应记录 prompt token，实际 %d", sum.PromptTokens)
			}
			if sum.CompletionTokens <= 0 {
				t.Errorf("应记录 completion token，实际 %d", sum.CompletionTokens)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("用量未被记录")
}

// TestStreamingPath 验证流式解析。
// QQ 不支持编辑消息，所以主链路不用流式；但流式接口要能用，
// 面板的「测试对话」就是走流式拿首字延迟的。
func TestStreamingPath(t *testing.T) {
	ls, err := mock.NewLLM(mock.LLMOptions{APIKey: "sk-mock", Reply: "流式返回的内容在这里。"})
	if err != nil {
		t.Fatalf("启动 mock 模型失败: %v", err)
	}
	defer ls.Close()

	client := llm.New()
	cfg := llm.Config{
		BaseURL:     ls.URL(),
		APIKey:      "sk-mock",
		Model:       "deepseek-flash",
		Temperature: 1.0,
		MaxTokens:   512,
		Timeout:     10 * time.Second,
	}

	var deltas int
	res, err := client.ChatStream(context.Background(), cfg, []llm.Message{
		{Role: "user", Content: "你好"},
	}, func(string) { deltas++ })
	if err != nil {
		t.Fatalf("流式调用失败: %v", err)
	}
	if res.Content != "流式返回的内容在这里。" {
		t.Errorf("内容不符，实际 %q", res.Content)
	}
	if deltas < 2 {
		t.Errorf("应分多块返回，实际 %d 块", deltas)
	}
	if res.Usage.CompletionTokens <= 0 {
		t.Error("流式响应应带回 usage")
	}
}
