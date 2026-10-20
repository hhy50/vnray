package vmess

// VMess 载荷的分块框架：每块 = 长度字段 + （可选认证的）载荷 + （可选明文填充）。
// 长度字段的编解码由 sizeCodec 决定（明文 / shake 掩码 / authLen AEAD），
// 载荷的加解密由 chunkAuth 决定（AES-128-GCM / ChaCha20-Poly1305 / 无加密）。

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"sync"

	"github.com/hhy50/vnray/internal/logging"
	"golang.org/x/crypto/sha3"
)

// maxChunkPayload 是单块载荷明文的上限（16 KiB，远小于长度字段 2 字节的表达范围）。
const maxChunkPayload = 16 * 1024

// debugChunks 临时调试开关。
var debugChunks = os.Getenv("VMESS_DEBUG") != ""

// sizeCodec 决定长度字段：decode 返回该块在长度字段之后占用的总字节数
// （载荷明文 + 载荷认证开销 + 填充），encode 为其逆操作。
type sizeCodec interface {
	sizeBytes() int
	decode(b []byte) (uint16, error)
	encode(size uint16, b []byte) []byte
	// nextPadding 仅在启用 GlobalPadding 时被调用（shake 流同时供掩码与填充）。
	nextPadding() uint16
}

// plainSizeCodec 明文 2 字节长度，无填充。
type plainSizeCodec struct{}

func (plainSizeCodec) sizeBytes() int                  { return 2 }
func (plainSizeCodec) decode(b []byte) (uint16, error) { return binary.BigEndian.Uint16(b), nil }
func (plainSizeCodec) encode(size uint16, b []byte) []byte {
	binary.BigEndian.PutUint16(b, size)
	return b[:2]
}
func (plainSizeCodec) nextPadding() uint16 { return 0 }

// shakeSizeCodec 用 SHAKE128(IV) 流做长度掩码与填充长度生成（OptionChunkMasking /
// OptionGlobalPadding）。每块按 填充→掩码 的顺序消费流，与规范一致。
type shakeSizeCodec struct{ shake sha3.ShakeHash }

func newShakeSizeCodec(nonce []byte) *shakeSizeCodec {
	s := sha3.NewShake128()
	_, _ = s.Write(nonce)
	return &shakeSizeCodec{shake: s}
}

func (s *shakeSizeCodec) sizeBytes() int { return 2 }

func (s *shakeSizeCodec) next() uint16 {
	var b [2]byte
	_, _ = s.shake.Read(b[:])
	return binary.BigEndian.Uint16(b[:])
}

func (s *shakeSizeCodec) decode(b []byte) (uint16, error) {
	mask := s.next()
	return binary.BigEndian.Uint16(b) ^ mask, nil
}

func (s *shakeSizeCodec) encode(size uint16, b []byte) []byte {
	mask := s.next()
	binary.BigEndian.PutUint16(b, mask^size)
	return b[:2]
}

func (s *shakeSizeCodec) nextPadding() uint16 { return s.next() % 64 }

// chunkAuth 是带递增 nonce 的 AEAD 认证器：nonce 前 2 字节为块序号（BE 递增），
// 其余取自会话 IV；附加认证数据为空。
type chunkAuth struct {
	aead  cipher.AEAD
	iv    []byte // 16 字节会话 IV
	count uint16
	mu    sync.Mutex
}

func newChunkAuth(aead cipher.AEAD, iv []byte) *chunkAuth {
	return &chunkAuth{aead: aead, iv: iv}
}

func (a *chunkAuth) overhead() int { return a.aead.Overhead() }

func (a *chunkAuth) nextNonce() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := make([]byte, a.aead.NonceSize())
	binary.BigEndian.PutUint16(n, a.count)
	copy(n[2:], a.iv[2:])
	a.count++
	if a.count == math.MaxUint16 {
		a.count = 0
	}
	return n
}

func (a *chunkAuth) open(cipherText []byte) ([]byte, error) {
	return a.aead.Open(nil, a.nextNonce(), cipherText, nil)
}

func (a *chunkAuth) seal(plain []byte) []byte {
	return a.aead.Seal(nil, a.nextNonce(), plain, nil)
}

// chunkReader 从原始流读取并解密分块载荷；对端半关闭（EOF）或终结块都会让
// Read 返回 io.EOF。
type chunkReader struct {
	r       io.Reader
	auth    *chunkAuth
	codec   sizeCodec
	padding bool
	done    bool
	buf     []byte
	pos     int
}

func newChunkReader(r io.Reader, auth *chunkAuth, codec sizeCodec, padding bool) *chunkReader {
	return &chunkReader{r: r, auth: auth, codec: codec, padding: padding}
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for r.pos == len(r.buf) {
		if r.done {
			return 0, io.EOF
		}
		if err := r.nextChunk(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	return n, nil
}

func (r *chunkReader) nextChunk() error {
	padding := uint16(0)
	if r.padding {
		padding = r.codec.nextPadding()
	}
	raw := make([]byte, r.codec.sizeBytes())
	if _, err := io.ReadFull(r.r, raw); err != nil {
		return err
	}
	size, err := r.codec.decode(raw)
	if err != nil {
		return err
	}
	if debugChunks {
		logging.Warnf("chunk read: size=%d padding=%d", size, padding)
	}
	if int(size) == r.auth.overhead()+int(padding) {
		r.done = true
		return io.EOF
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r.r, data); err != nil {
		return err
	}
	if padding > 0 {
		size -= padding
	}
	plain, err := r.auth.open(data[:size])
	if err != nil {
		return err
	}
	r.buf = plain
	r.pos = 0
	return nil
}

// nextPayload 读取下一个完整分块的明文并作为单个数据报返回。终结块或对端半关闭返回 io.EOF；
// 返回的切片在下次 nextChunk 前有效（nextChunk 会重新分配）。
func (r *chunkReader) nextPayload() ([]byte, error) {
	for r.pos == len(r.buf) {
		if r.done {
			return nil, io.EOF
		}
		if err := r.nextChunk(); err != nil {
			return nil, err
		}
	}
	payload := r.buf[r.pos:]
	r.pos = len(r.buf)
	return payload, nil
}

// chunkWriter 把明文写入封装成分块加密帧；Close 写出终结块（空载荷）。
type chunkWriter struct {
	mu      sync.Mutex
	w       io.Writer
	auth    *chunkAuth
	codec   sizeCodec
	padding bool
}

func newChunkWriter(w io.Writer, auth *chunkAuth, codec sizeCodec, padding bool) *chunkWriter {
	return &chunkWriter{w: w, auth: auth, codec: codec, padding: padding}
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

// Close 写出终结块（空载荷分块），对端读到即视为请求/响应结束。
func (w *chunkWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sealSized(nil, 0)
}

// maxPacketSize 是单个数据报封成块的上限（长度字段为 uint16 满值）。
const maxPacketSize = 65535

// WritePacket 把单个数据报封装成一个分块写出（不经 Write 的 16K 分包，保持数据报唯一）。
// 长度超限（含认证开销与填充）时拒绝返回错误。
func (w *chunkWriter) WritePacket(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sealSized(payload, maxPacketSize)
}

func (w *chunkWriter) seal(plain []byte) error { return w.sealSized(plain, 0) }

// sealSized 把明文封成一个分块写出；maxSize>0 时校验 size 不超限（Write 的字节流分包不设限，
// 单个数据报封块受长度字段上限约束）。
func (w *chunkWriter) sealSized(plain []byte, maxSize int) error {
	padding := uint16(0)
	if w.padding {
		padding = w.codec.nextPadding()
	}
	size := len(plain) + w.auth.overhead() + int(padding)
	if maxSize > 0 && size > maxSize {
		return fmt.Errorf("vmess: datagram too large for one chunk: %d > %d", size, maxSize)
	}
	if debugChunks {
		logging.Warnf("chunk write: size=%d padding=%d payload=%d", size, padding, len(plain))
	}
	field := make([]byte, w.codec.sizeBytes())
	w.codec.encode(uint16(size), field)
	if _, err := w.w.Write(field); err != nil {
		return err
	}
	if _, err := w.w.Write(w.auth.seal(plain)); err != nil {
		return err
	}
	if padding > 0 {
		pad := make([]byte, padding)
		if _, err := rand.Read(pad); err != nil {
			return err
		}
		if _, err := w.w.Write(pad); err != nil {
			return err
		}
	}
	return nil
}
