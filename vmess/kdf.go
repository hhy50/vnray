package vmess

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"hash"
	"hash/fnv"
)

// KDF 派生 AEAD 密钥材料：以外层盐开始的嵌套 HMAC-SHA256 链，
// 每个 path 把前一级 HMAC 实例当作下一级的基础哈希，最终对 key 求值。
func KDF(key []byte, path ...string) []byte {
	hmacf := hmac.New(sha256.New, []byte(KDFSaltConstVMessAEADKDF))

	for _, v := range path {
		first := true
		hmacf = hmac.New(func() hash.Hash {
			if first {
				first = false
				return hmacHash{hmacf}
			}
			return hmacf
		}, []byte(v))
	}
	hmacf.Write(key)
	return hmacf.Sum(nil)
}

// KDF16 返回 KDF 结果的前 16 字节（AES-128 密钥长度）。
func KDF16(key []byte, path ...string) []byte {
	return KDF(key, path...)[:16]
}

// hmacHash 让一个现成的 HMAC 实例充当下一级 HMAC 的基础哈希。
type hmacHash struct{ hash.Hash }

// CmdKey 从 UUID 字节派生认证密钥（MD5），所有 AEAD 运算的根密钥。
func CmdKey(uuidBytes []byte) []byte {
	h := md5.New()
	_, _ = h.Write(uuidBytes)
	_, _ = h.Write([]byte(cmdKeySalt))
	return h.Sum(nil)
}

// GenerateChacha20Poly1305Key 从 16 字节密钥扩展出 32 字节 ChaCha20-Poly1305 密钥。
func GenerateChacha20Poly1305Key(b []byte) []byte {
	key := make([]byte, 32)
	t := md5.Sum(b)
	copy(key, t[:])
	t = md5.Sum(key[:16])
	copy(key[16:], t[:])
	return key
}

// fnvNew32a 供请求头校验与测试使用。
func fnvNew32a() hash.Hash32 { return fnv.New32a() }
