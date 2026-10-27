// Package secret 提供对称加密与随机凭据生成。
//
// 用途：面板里录入的 LLM API Key 需要落库，但不能明文存。
// 用 AES-256-GCM 加密，主密钥来自 etc/secrets.env 的 QQBOT_SECRET_KEY，
// 与数据库分开存放——只拿到数据库文件无法解密。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize 是主密钥长度（AES-256）。
const KeySize = 32

// Box 是一个 AES-256-GCM 的加解密器。
type Box struct {
	aead cipher.AEAD
}

// New 用十六进制主密钥构造 Box。
func New(keyHex string) (*Box, error) {
	keyHex = strings.TrimSpace(keyHex)
	if keyHex == "" {
		return nil, errors.New("主密钥为空")
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("主密钥不是合法十六进制: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("主密钥长度应为 %d 字节，实际 %d 字节", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Encrypt 加密明文，返回 base64(nonce||ciphertext)。
func (b *Box) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt 解密 Encrypt 的输出。
func (b *Box) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("密文不是合法 base64: %w", err)
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("密文长度不足")
	}
	plain, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", errors.New("解密失败（主密钥可能已更换）")
	}
	return string(plain), nil
}

// GenerateKey 生成一个新的十六进制主密钥。
func GenerateKey() (string, error) {
	buf := make([]byte, KeySize)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// GenerateToken 生成 n 字节的十六进制随机串，用于面板密码、WS token 等。
func GenerateToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Equal 做常数时间比较，避免时序侧信道。
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
