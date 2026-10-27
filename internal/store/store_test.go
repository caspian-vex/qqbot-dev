package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vex/qqbot/internal/secret"
)

func newTestStore(t *testing.T) (*Store, *secret.Box) {
	t.Helper()
	key, err := secret.GenerateKey()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	box, err := secret.New(key)
	if err != nil {
		t.Fatalf("构造加密器失败: %v", err)
	}
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	st.SetBox(box)
	t.Cleanup(func() { st.Close() })
	return st, box
}

func TestDefaultSettings(t *testing.T) {
	st, _ := newTestStore(t)
	s, err := st.Settings()
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	if s.LLM.Model != "deepseek-flash" {
		t.Errorf("默认模型应为 deepseek-flash，实际 %q", s.LLM.Model)
	}
	if s.LLM.Thinking {
		t.Error("思考模式默认应为关闭——开启会让每条消息多几秒延迟")
	}
	if s.Bot.PassiveRate != 0.15 {
		t.Errorf("默认被动概率应为 0.15，实际 %v", s.Bot.PassiveRate)
	}
}

func TestAPIKeyEncryptedAtRest(t *testing.T) {
	st, _ := newTestStore(t)
	plain := "sk-secret-key-value-123"

	if _, err := st.UpdateSettings(func(v *Settings) error {
		v.LLM.APIKey = plain
		return nil
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// 直接从数据库读原始 JSON，确认不是明文
	var raw string
	if err := st.db.QueryRow(`SELECT json FROM app_settings WHERE id = 1`).Scan(&raw); err != nil {
		t.Fatalf("读取原始行失败: %v", err)
	}
	if strings.Contains(raw, plain) {
		t.Fatal("数据库里出现了明文 API Key")
	}
	if !strings.Contains(raw, encPrefix) {
		t.Fatal("数据库里没有加密前缀，说明未加密")
	}

	// 从 API 读应能解密回来
	got, err := st.Settings()
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	if got.LLM.APIKey != plain {
		t.Errorf("解密结果不符：期望 %q，实际 %q", plain, got.LLM.APIKey)
	}
}

func TestSettingsPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	key, _ := secret.GenerateKey()
	box, _ := secret.New(key)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	st.SetBox(box)
	if _, err := st.UpdateSettings(func(v *Settings) error {
		v.Bot.PassiveRate = 0.42
		v.LLM.APIKey = "sk-persist"
		return nil
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	defer st2.Close()
	st2.SetBox(box)

	got, err := st2.Settings()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Bot.PassiveRate != 0.42 {
		t.Errorf("设置未持久化：%v", got.Bot.PassiveRate)
	}
	if got.LLM.APIKey != "sk-persist" {
		t.Errorf("API Key 未正确解密：%q", got.LLM.APIKey)
	}
}

func TestSettingsSlicesNeverNull(t *testing.T) {
	st, _ := newTestStore(t)

	// 触发一次写入：空切片经过 append 拷贝很容易退化成 nil，
	// 一旦序列化成 null，前端对 wake_words 调 .map 会直接崩。
	if _, err := st.UpdateSettings(func(v *Settings) error { return nil }); err != nil {
		t.Fatalf("写入设置失败: %v", err)
	}

	s, err := st.Settings()
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	if s.Bot.WakeWords == nil {
		t.Error("wake_words 不应为 nil")
	}
	if s.Bot.WhitelistGroups == nil {
		t.Error("whitelist_groups 不应为 nil")
	}
	if s.Bot.BlacklistUsers == nil {
		t.Error("blacklist_users 不应为 nil")
	}

	blob, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("序列化设置失败: %v", err)
	}
	for _, key := range []string{"wake_words", "whitelist_groups", "blacklist_users"} {
		if strings.Contains(string(blob), `"`+key+`":null`) {
			t.Errorf("%s 被序列化成了 null: %s", key, blob)
		}
	}
}

func TestSettingsReturnedCopyIsMutable(t *testing.T) {
	st, _ := newTestStore(t)

	// 调用方改返回值不应污染内部缓存，
	// 否则一次误改会让整个进程后续读到错的配置。
	s1, err := st.Settings()
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	s1.LLM.Model = "被改坏了"

	s2, err := st.Settings()
	if err != nil {
		t.Fatalf("再次读取设置失败: %v", err)
	}
	if s2.LLM.Model == "被改坏了" {
		t.Error("Settings() 返回的应是副本，不应影响缓存")
	}
}

func TestSessionAndMessages(t *testing.T) {
	st, _ := newTestStore(t)
	sid := SessionID("group", 12345)

	if err := st.TouchSession(sid, "group", 12345); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	for _, m := range []struct{ role, text string }{
		{"user", "你好"},
		{"assistant", "你好呀"},
		{"user", "今天怎么样"},
	} {
		if err := st.AppendMessage(sid, m.role, m.text, 3); err != nil {
			t.Fatalf("写入消息失败: %v", err)
		}
	}

	msgs, err := st.RecentMessages(sid, 10)
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("期望 3 条，实际 %d 条", len(msgs))
	}
	// 必须是时间正序
	if msgs[0].Content != "你好" || msgs[2].Content != "今天怎么样" {
		t.Errorf("消息顺序错误: %+v", msgs)
	}

	// 只取最近 2 条
	recent, _ := st.RecentMessages(sid, 2)
	if len(recent) != 2 || recent[1].Content != "今天怎么样" {
		t.Errorf("截断结果错误: %+v", recent)
	}

	if err := st.ClearSession(sid); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	n, _ := st.CountMessages(sid)
	if n != 0 {
		t.Errorf("清空后应为 0 条，实际 %d", n)
	}
}

func TestPersona(t *testing.T) {
	st, _ := newTestStore(t)

	p, err := st.ActivePersona()
	if err != nil {
		t.Fatalf("获取默认人格失败: %v", err)
	}
	if p.SystemPrompt != DefaultSystemPrompt {
		t.Errorf("默认人格应为 %q，实际 %q", DefaultSystemPrompt, p.SystemPrompt)
	}
	if !p.Active {
		t.Error("默认人格应处于启用状态")
	}

	second, err := st.UpsertPersona("catgirl", "你是一只猫娘")
	if err != nil {
		t.Fatalf("创建人格失败: %v", err)
	}
	if second.Active {
		t.Error("新建人格不应自动启用")
	}

	if err := st.ActivatePersona(second.ID); err != nil {
		t.Fatalf("切换人格失败: %v", err)
	}
	active, _ := st.ActivePersona()
	if active.Name != "catgirl" {
		t.Errorf("切换后应为人格 catgirl，实际 %q", active.Name)
	}

	// 同一个名字再写应更新而不是新建
	updated, err := st.UpsertPersona("catgirl", "你是一只很凶的猫娘")
	if err != nil {
		t.Fatalf("更新人格失败: %v", err)
	}
	if updated.ID != second.ID {
		t.Error("同名人格应更新而非新建")
	}
	list, _ := st.ListPersonas()
	if len(list) != 2 {
		t.Errorf("应有 2 个人格，实际 %d", len(list))
	}
}

func TestUsageCost(t *testing.T) {
	st, _ := newTestStore(t)

	// 空闲时段：100 万输入 + 100 万输出 = 1 + 4 = 5 元
	if err := st.RecordUsage("deepseek-flash", 1_000_000, 1_000_000, false); err != nil {
		t.Fatalf("记录用量失败: %v", err)
	}
	day := time.Now().Format("2006-01-02")
	sum, err := st.UsageForDay(day)
	if err != nil {
		t.Fatalf("汇总失败: %v", err)
	}
	if sum.Requests != 1 {
		t.Errorf("请求数应为 1，实际 %d", sum.Requests)
	}
	if sum.CostCNY < 4.99 || sum.CostCNY > 5.01 {
		t.Errorf("空闲时段费用应约 5 元，实际 %.4f", sum.CostCNY)
	}

	// 高峰时段应翻倍
	st2, _ := newTestStore(t)
	_ = st2.RecordUsage("deepseek-flash", 1_000_000, 1_000_000, true)
	sum2, _ := st2.UsageForDay(time.Now().Format("2006-01-02"))
	if sum2.CostCNY < 9.99 || sum2.CostCNY > 10.01 {
		t.Errorf("高峰时段费用应约 10 元，实际 %.4f", sum2.CostCNY)
	}
}

func TestAudit(t *testing.T) {
	st, _ := newTestStore(t)
	st.RecordAudit("login.ok", "登录成功", "1.2.3.4")
	st.RecordAudit("settings.updated", "改了概率", "1.2.3.4")

	list, err := st.ListAudit(10)
	if err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("期望 2 条，实际 %d", len(list))
	}
	// 倒序：最新的在前
	if list[0].Action != "settings.updated" {
		t.Errorf("审计应按时间倒序，实际第一条是 %q", list[0].Action)
	}
}
