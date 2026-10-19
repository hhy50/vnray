package shadowsocks

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Method 是一种 AEAD 加密方法：主密钥长度与盐长度。
type Method struct {
	// Name 是方法的规范名（配置与展示用）。
	Name string
	// KeyLen 是主密钥（及子密钥）长度。
	KeyLen int
	// SaltLen 是每条连接会话盐的长度。
	SaltLen int
	newAEAD func(key []byte) (cipher.AEAD, error)
}

// 支持的 AEAD 方法表。旧式流式加密（rc4-md5 等）不在支持范围。
var (
	MethodAES128GCM = Method{Name: "aes-128-gcm", KeyLen: 16, SaltLen: 16, newAEAD: newAESGCM}
	MethodAES256GCM = Method{Name: "aes-256-gcm", KeyLen: 32, SaltLen: 32, newAEAD: newAESGCM}
	MethodChaCha20  = Method{Name: "chacha20-ietf-poly1305", KeyLen: 32, SaltLen: 32, newAEAD: newChaCha}
)

var methods = map[string]Method{
	MethodAES128GCM.Name: MethodAES128GCM,
	MethodAES256GCM.Name: MethodAES256GCM,
	MethodChaCha20.Name:  MethodChaCha20,
}

// ParseMethod 按规范名解析加密方法；接受客户端配置里的常见别名。
func ParseMethod(name string) (Method, error) {
	switch name {
	case "aes-128-gcm", "aead_aes_128_gcm":
		return MethodAES128GCM, nil
	case "aes-256-gcm", "aead_aes_256_gcm":
		return MethodAES256GCM, nil
	case "chacha20-ietf-poly1305", "chacha20-poly1305", "aead_chacha20_poly1305":
		return MethodChaCha20, nil
	default:
		return Method{}, fmt.Errorf("shadowsocks: unsupported method %q (aead only)", name)
	}
}

// maxChunkPayload 是单块载荷明文上限（0x3FFF，shadowsocks AEAD 规范值）。
const maxChunkPayload = 0x3FFF

// tagSize 是 AEAD 认证标签长度（三种方法的 GCM/Poly1305 都是 16 字节）。
const tagSize = 16

func newAESGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func newChaCha(key []byte) (cipher.AEAD, error) {
	return chacha20poly1305.New(key)
}

// evpBytesToKey 是 OpenSSL EVP_BytesToKey（MD5、无盐、count=1）的等价实现：
// 密码 → 主密钥，与 go-shadowsocks2 / v2ray-core 的 passwordToCipherKey 一致。
func evpBytesToKey(password []byte, keyLen int) []byte {
	sum := md5.Sum(password)
	out := make([]byte, 0, keyLen+md5.Size)
	out = append(out, sum[:]...)
	prev := sum[:]
	for len(out) < keyLen {
		h := md5.New()
		_, _ = h.Write(prev)
		_, _ = h.Write(password)
		prev = h.Sum(nil)
		out = append(out, prev...)
	}
	return out[:keyLen]
}

// subkey 派生会话子密钥：HKDF-SHA1(主密钥, 盐, info="ss-subkey")，长度同主密钥。
func subkey(master, salt []byte, keyLen int) []byte {
	r := hkdf.New(sha1.New, master, salt, []byte("ss-subkey"))
	out := make([]byte, keyLen)
	if _, err := io.ReadFull(r, out); err != nil {
		// HKDF 的输出长度足够时不会失败；走到这里说明参数错误，直接 panic。
		panic(fmt.Sprintf("shadowsocks: derive subkey: %v", err))
	}
	return out
}

// nonceGen 生成 12 字节小端计数 nonce：首个为全零，其后逐次加一。
// 与 v2ray 的 GenerateInitialAEADNonce 序列逐字节一致。
type nonceGen struct {
	n [12]byte
}

func newNonceGen() *nonceGen {
	g := &nonceGen{}
	// 起点全 0xFF，先加一再返回：首个 nonce 即全零。
	for i := range g.n {
		g.n[i] = 0xFF
	}
	return g
}

func (g *nonceGen) next() []byte {
	for i := range g.n {
		g.n[i]++
		if g.n[i] != 0 {
			break
		}
	}
	return g.n[:]
}

// chunkReader 解密分块载荷：每块 = [2B 密文长度 + 16B 长度标签] + [密文载荷 + 16B 载荷标签]。
// 长0 的块视为协议错误（shadowsocks 没有带内结束块，结束由 TCP FIN 表达）。
type chunkReader struct {
	r     io.Reader
	aead  cipher.AEAD
	nonce *nonceGen
	buf   []byte
	pos   int
}

func newChunkReader(r io.Reader, aead cipher.AEAD) *chunkReader {
	return &chunkReader{r: r, aead: aead, nonce: newNonceGen()}
}

// Read 读取解密后的载荷；读到对端半关闭（EOF）或解密失败为止。
func (r *chunkReader) Read(p []byte) (int, error) {
	for r.pos == len(r.buf) {
		if err := r.nextChunk(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	return n, nil
}

func (r *chunkReader) nextChunk() error {
	block := make([]byte, 2+tagSize)
	if _, err := io.ReadFull(r.r, block); err != nil {
		return err
	}
	sizeField, err := r.aead.Open(nil, r.nonce.next(), block, nil)
	if err != nil {
		return fmt.Errorf("shadowsocks: open chunk length: %w", err)
	}
	size := int(binary.BigEndian.Uint16(sizeField))
	if size == 0 {
		return io.ErrUnexpectedEOF
	}
	if size > maxChunkPayload {
		return fmt.Errorf("shadowsocks: chunk size %d over limit", size)
	}
	payload := make([]byte, size+tagSize)
	if _, err := io.ReadFull(r.r, payload); err != nil {
		return err
	}
	plain, err := r.aead.Open(nil, r.nonce.next(), payload, nil)
	if err != nil {
		return fmt.Errorf("shadowsocks: open chunk payload: %w", err)
	}
	r.buf = plain
	r.pos = 0
	return nil
}

// chunkWriter 加密分块载荷：Write 的数据按块上限切片逐块加密写出。
type chunkWriter struct {
	mu    sync.Mutex
	w     io.Writer
	aead  cipher.AEAD
	nonce *nonceGen
}

func newChunkWriter(w io.Writer, aead cipher.AEAD) *chunkWriter {
	return &chunkWriter{w: w, aead: aead, nonce: newNonceGen()}
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxChunkPayload)
		if err := w.seal(p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (w *chunkWriter) seal(plain []byte) error {
	sizeField := make([]byte, 2)
	binary.BigEndian.PutUint16(sizeField, uint16(len(plain)))
	out := make([]byte, 0, len(sizeField)+tagSize+len(plain)+tagSize)
	out = w.aead.Seal(out, w.nonce.next(), sizeField, nil)
	out = w.aead.Seal(out, w.nonce.next(), plain, nil)
	_, err := w.w.Write(out)
	return err
}
