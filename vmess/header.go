package vmess

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
)

// parseUUID 解析标准 36 字符 UUID（8-4-4-4-12，连字符分隔）为 16 字节。
func parseUUID(s string) ([16]byte, error) {
	var out [16]byte
	s = strings.TrimSpace(s)
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return out, fmt.Errorf("vmess: invalid uuid format %q", s)
	}
	hex := strings.ReplaceAll(s, "-", "")
	if len(hex) != 32 {
		return out, errors.New("vmess: invalid uuid length")
	}
	for i := 0; i < 32; i++ {
		c := hex[i]
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		default:
			return out, fmt.Errorf("vmess: invalid uuid hex %q", string(c))
		}
		if i%2 == 0 {
			out[i/2] = v << 4
		} else {
			out[i/2] |= v
		}
	}
	return out, nil
}

// openAEADHeader 解开 AEAD 请求头：authID(16) + 长度块(2+16) + nonce(8) + 载荷(len+16)。
// 返回解密后的明文头（38B 请求头 + 端口/地址 + 填充 + 4B FNV 校验）。
func openAEADHeader(cmdKey []byte, authID authID, r io.Reader) ([]byte, error) {
	var lengthBlock [18]byte
	var nonce [8]byte
	if _, err := io.ReadFull(r, lengthBlock[:]); err != nil {
		return nil, fmt.Errorf("vmess: read aead header length: %w", err)
	}
	if _, err := io.ReadFull(r, nonce[:]); err != nil {
		return nil, fmt.Errorf("vmess: read aead header nonce: %w", err)
	}

	gcm := newAESGCM(KDF16(cmdKey, KDFSaltConstVMessHeaderPayloadLengthAEADKey, string(authID[:]), string(nonce[:])))
	nonce12 := KDF(cmdKey, KDFSaltConstVMessHeaderPayloadLengthAEADIV, string(authID[:]), string(nonce[:]))[:12]
	lengthPlain, err := gcm.Open(nil, nonce12, lengthBlock[:], authID[:])
	if err != nil {
		return nil, fmt.Errorf("vmess: open aead header length: %w", err)
	}
	if len(lengthPlain) != 2 {
		return nil, errors.New("vmess: bad aead header length payload")
	}
	length := int(lengthPlain[0])<<8 | int(lengthPlain[1])

	payload := make([]byte, length+16)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("vmess: read aead header payload: %w", err)
	}

	gcm = newAESGCM(KDF16(cmdKey, KDFSaltConstVMessHeaderPayloadAEADKey, string(authID[:]), string(nonce[:])))
	nonce12 = KDF(cmdKey, KDFSaltConstVMessHeaderPayloadAEADIV, string(authID[:]), string(nonce[:]))[:12]
	plain, err := gcm.Open(nil, nonce12, payload, authID[:])
	if err != nil {
		return nil, fmt.Errorf("vmess: open aead header payload: %w", err)
	}
	return plain, nil
}

// sealAEADHeader 是 openAEADHeader 的逆操作（测试客户端模拟用）。
func sealAEADHeader(cmdKey []byte, authID authID, plain []byte) []byte {
	nonce := make([]byte, 8)
	if _, err := randRead(nonce); err != nil {
		panic(err)
	}

	gcm := newAESGCM(KDF16(cmdKey, KDFSaltConstVMessHeaderPayloadLengthAEADKey, string(authID[:]), string(nonce[:])))
	nonce12 := KDF(cmdKey, KDFSaltConstVMessHeaderPayloadLengthAEADIV, string(authID[:]), string(nonce[:]))[:12]
	lenPlain := []byte{byte(len(plain) >> 8), byte(len(plain))}
	lengthSealed := gcm.Seal(nil, nonce12, lenPlain, authID[:])

	gcm = newAESGCM(KDF16(cmdKey, KDFSaltConstVMessHeaderPayloadAEADKey, string(authID[:]), string(nonce[:])))
	nonce12 = KDF(cmdKey, KDFSaltConstVMessHeaderPayloadAEADIV, string(authID[:]), string(nonce[:]))[:12]
	payloadSealed := gcm.Seal(nil, nonce12, plain, authID[:])

	out := make([]byte, 0, 16+18+8+len(payloadSealed))
	out = append(out, authID[:]...)
	out = append(out, lengthSealed...)
	out = append(out, nonce[:]...)
	out = append(out, payloadSealed...)
	return out
}

// newAESGCM 构造 AES-128-GCM（密钥必须 16 字节，NonceSize 12）。
func newAESGCM(key []byte) cipher.AEAD {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return g
}

// randRead 优先使用 crypto/rand。
var randRead = rand.Read
