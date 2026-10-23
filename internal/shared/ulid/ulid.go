// Package ulid 生成 ULID 兼容的标识符，不依赖数据库序列。
package ulid

import (
	"crypto/rand"
	"fmt"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New 基于给定时间与密码学随机源返回 26 位 Crockford Base32 ULID。
// 时间部分取 UTC 毫秒，使生成的标识符近似按时间有序。
func New(now time.Time) (string, error) {
	milliseconds := now.UTC().UnixMilli()
	if milliseconds < 0 || milliseconds > 0xFFFFFFFFFFFF {
		return "", fmt.Errorf("ULID timestamp is out of range")
	}

	var raw [16]byte
	raw[0] = byte(milliseconds >> 40)
	raw[1] = byte(milliseconds >> 32)
	raw[2] = byte(milliseconds >> 24)
	raw[3] = byte(milliseconds >> 16)
	raw[4] = byte(milliseconds >> 8)
	raw[5] = byte(milliseconds)
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", fmt.Errorf("read ULID randomness: %w", err)
	}

	var encoded [26]byte
	for index := range encoded {
		var value byte
		for bit := 0; bit < 5; bit++ {
			value <<= 1
			sourceBit := index*5 + bit - 2 // ULID 的首个 Base32 字符前导两位恒为 0。
			if sourceBit < 0 || sourceBit >= len(raw)*8 {
				continue
			}
			current := (raw[sourceBit/8] >> (7 - sourceBit%8)) & 1
			value |= current
		}
		encoded[index] = alphabet[value]
	}

	return string(encoded[:]), nil
}

// Generator 将 New 适配为需要标识符生成器的应用依赖。
type Generator struct{}

// New 按给定时间戳生成 ULID。
func (Generator) New(now time.Time) (string, error) {
	return New(now)
}
