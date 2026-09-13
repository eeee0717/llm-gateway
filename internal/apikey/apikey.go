// Package apikey 负责 API Key 的生成、哈希、鉴权、鉴权缓存和存取。
// 数据库里只存 Key 的 SHA-256，明文只在创建时返回一次，见 docs/adr/0003。
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// prefix 是 API Key 的固定前缀，方便一眼认出这是网关发的 Key。
const prefix = "sk-"

// Generate 生成一个新的 API Key，返回明文和它的哈希。明文只在这里出现一次，之后只剩哈希。
func Generate() (plain, hash string) {
	var b [32]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read 不会返回错误，读不到随机数时它自己 panic
	plain = prefix + base64.RawURLEncoding.EncodeToString(b[:])
	return plain, Hash(plain)
}

// Hash 返回 Key 的 SHA-256，十六进制。Key 本身是 256 位随机数，不需要加盐的慢哈希。
func Hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
