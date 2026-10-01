package vcs

import (
	"crypto/sha256"
	"encoding/hex"
)

// hashBytes 返回数据的 SHA-256 十六进制摘要（64 个字符）。
func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func isHexID(s string) bool {
	if len(s)) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
