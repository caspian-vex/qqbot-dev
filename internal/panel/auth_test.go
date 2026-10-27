package panel

import (
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	pw := "correct-horse-battery-staple"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}
	if !VerifyPassword(pw, hash) {
		t.Error("正确密码校验失败")
	}
	if VerifyPassword("wrong-password", hash) {
		t.Error("错误密码竟然通过了校验")
	}
	if VerifyPassword(pw, "") {
		t.Error("空哈希不应通过")
	}
	if VerifyPassword(pw, "garbage") {
		t.Error("非法哈希不应通过")
	}
}

func TestPasswordHashUnique(t *testing.T) {
	h1, _ := HashPassword("same")
	h2, _ := HashPassword("same")
	if h1 == h2 {
		t.Error("相同密码两次哈希应因随机盐而不同")
	}
}

func TestTOTP(t *testing.T) {
	// RFC 6238 测试向量（SHA1，密钥 "12345678901234567890" 的 base32）
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

	// 时间点 59 秒对应计数器 1，RFC 期望 94287082 的后 6 位 → 287082
	got, err := TOTPCode(secret, time.Unix(59, 0))
	if err != nil {
		t.Fatalf("计算 TOTP 失败: %v", err)
	}
	if got != "287082" {
		t.Errorf("RFC 6238 向量不符：期望 287082，实际 %s", got)
	}
}

func TestTOTPVerifyWindow(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	now, err := TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatalf("计算失败: %v", err)
	}
	if !VerifyTOTP(secret, now) {
		t.Error("当前验证码应通过")
	}
	// 上一时间窗的码也应通过（容忍时钟漂移）
	prev, _ := TOTPCode(secret, time.Now().Add(-30*time.Second))
	if !VerifyTOTP(secret, prev) {
		t.Error("上一个时间窗的验证码应通过")
	}
	// 太早的码不应通过
	old, _ := TOTPCode(secret, time.Now().Add(-10*time.Minute))
	if VerifyTOTP(secret, old) {
		t.Error("10 分钟前的验证码不应通过")
	}
	if VerifyTOTP(secret, "000000") {
		// 极小概率误命中，忽略
		t.Log("警告：000000 意外命中，可忽略")
	}
	if VerifyTOTP(secret, "12345") {
		t.Error("长度不足不应通过")
	}
}

func TestMaskKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"short", "****"},
		{"sk-1234567890abcdef", "sk-1****cdef"},
	}
	for _, tt := range tests {
		if got := maskKey(tt.in); got != tt.want {
			t.Errorf("maskKey(%q) = %q，期望 %q", tt.in, got, tt.want)
		}
	}
}

func TestSubtleEqual(t *testing.T) {
	if !subtleEqual("abc", "abc") {
		t.Error("相同字符串应相等")
	}
	if subtleEqual("abc", "abd") {
		t.Error("不同字符串不应相等")
	}
	if subtleEqual("abc", "ab") {
		t.Error("长度不同不应相等")
	}
}
