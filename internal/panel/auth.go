package panel

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// argon2id 参数。在 1 核机器上刻意取保守值：
// 64MB 内存、3 轮迭代，登录一次约 100ms，足够抗离线爆破，
// 又不会在登录瞬间把仅有的一核占满影响机器人回复。
const (
	argonMemory  = 64 * 1024
	argonTime    = 3
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword 生成 argon2id 哈希，格式：argon2id$v=19$m=65536,t=3,p=2$salt$hash
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword 校验密码。
func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[1], "v=%d", &version); err != nil {
		return false
	}
	var mem uint32
	var iters uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &mem, &iters, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iters, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---- TOTP (RFC 6238) ----
//
// 自己实现而不是引第三方库：只有 40 行，且能避免为一个功能引入依赖。

// NewTOTPSecret 生成一个新的 base32 密钥（不带 padding）。
func NewTOTPSecret() (string, error) {
	buf := make([]byte, 20) // 160 bit，RFC 4226 推荐
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// TOTPCode 计算指定时刻的 6 位验证码。
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).
		DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("TOTP 密钥不合法: %w", err)
	}
	counter := uint64(t.Unix() / 30)

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])

	return fmt.Sprintf("%06d", code%1_000_000), nil
}

// VerifyTOTP 校验验证码，允许前后各一个时间窗（共 ±30 秒），容忍时钟漂移。
func VerifyTOTP(secret, code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	now := time.Now()
	for _, delta := range []time.Duration{0, -30 * time.Second, 30 * time.Second} {
		want, err := TOTPCode(secret, now.Add(delta))
		if err != nil {
			return false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// TOTPProvisioningURL 生成 otpauth:// 链接，用于二维码。
func TOTPProvisioningURL(issuer, account, secret string) string {
	return fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s&algorithm=SHA1&digits=6&period=30",
		urlEscape(issuer), urlEscape(account), secret, urlEscape(issuer))
}

func urlEscape(s string) string {
	// 极简转义：issuer/account 一般只有字母数字和少量符号
	r := strings.NewReplacer(" ", "%20", ":", "%3A", "/", "%2F", "@", "%40")
	return r.Replace(s)
}

// randomToken 生成 n 字节的 URL 安全随机串。
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// sha256Hex 用于会话令牌的存储摘要：即使内存被 dump 也拿不到原始令牌。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
