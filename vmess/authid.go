package vmess

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"sync"
	"sync/atomic"
	"time"
)

// authIDMaxDelta 是 AuthID 时间戳允许的偏差；antireplayWindow 是重放窗口，两者同为 120s。
const (
	authIDMaxDelta   = 120
	replayWindowSecs = 120
)

var (
	errAuthIDUserNotFound = errors.New("vmess: user not found for auth id")
	errAuthIDInvalidTime  = errors.New("vmess: invalid auth id timestamp")
	errAuthIDReplay       = errors.New("vmess: replayed auth id")
)

// authID 由 16 字节组成：8B BE 时间戳 + 4B 随机 + 4B CRC32 校验，整体经 AES-ECB 加密。
type authID [16]byte

// newAuthID 用 cmdKey 生成指定时刻的 AuthID（服务端测试/管理端模拟用）。
func newAuthID(cmdKey []byte, at time.Time) authID {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], uint64(at.Unix()))
	if _, err := rand.Read(buf[8:12]); err != nil {
		panic(err)
	}
	binary.BigEndian.PutUint32(buf[12:16], crc32.ChecksumIEEE(buf[:12]))

	block, _ := aes.NewCipher(KDF16(cmdKey, KDFSaltConstAuthIDEncryptionKey))
	var out authID
	block.Encrypt(out[:], buf[:])
	return out
}

// authPlainTime 校验解密后的 AuthID 明文：CRC 匹配且时间戳在窗口内才有效，
// 返回内嵌时间戳。调用方已用正确密钥解出明文。
func authPlainTime(plain *[16]byte, now time.Time) (time.Time, bool) {
	wantCRC := binary.BigEndian.Uint32(plain[12:16])
	if crc32.ChecksumIEEE(plain[:12]) != wantCRC {
		return time.Time{}, false
	}
	at := time.Unix(int64(binary.BigEndian.Uint64(plain[:8])), 0)
	if delta := now.Sub(at); delta > replayWindowSecs*time.Second || delta < -replayWindowSecs*time.Second {
		return time.Time{}, false
	}
	return at, true
}

// decodeAuthID 解密并校验 AuthID：返回内嵌时间戳。每次调用重做 KDF 派生与
// AES key schedule，只供测试与参考；认证热路径走 Validator.Match（块已预计算）。
func decodeAuthID(cmdKey []byte, id authID) (time.Time, error) {
	block, err := aes.NewCipher(KDF16(cmdKey, KDFSaltConstAuthIDEncryptionKey))
	if err != nil {
		return time.Time{}, err
	}
	var plain authID
	block.Decrypt(plain[:], id[:])
	at, ok := authPlainTime((*[16]byte)(&plain), time.Now())
	if !ok {
		return time.Time{}, errAuthIDInvalidTime
	}
	return at, nil
}

// replayFilter 阻断同一 AuthID 在重放窗口内的重复出现。
type replayFilter struct {
	seen   sync.Map
	lastGC atomic.Int64
}

func newReplayFilter() *replayFilter {
	f := &replayFilter{}
	f.lastGC.Store(time.Now().UnixNano())
	return f
}

// Check 记录并校验：窗口内重复出现返回 false。
func (f *replayFilter) Check(id authID) bool {
	key := id
	now := time.Now()
	if _, loaded := f.seen.LoadOrStore(key, now); loaded {
		return false
	}
	last := f.lastGC.Load()
	if now.UnixNano()-last > int64(time.Minute) && f.lastGC.CompareAndSwap(last, now.UnixNano()) {
		f.seen.Range(func(key, value any) bool {
			if now.Sub(value.(time.Time)) >= replayWindowSecs*time.Second {
				f.seen.CompareAndDelete(key, value)
			}
			return true
		})
	}
	return true
}

// user 是一个已注册的 VMess 账号：raw 为 UUID 的 16 字节原文，authBlock 是
// AuthID 认证用的 AES 块，密钥材料在注册时派生一次（热路径只做块解密）。
type user struct {
	uuid      string
	raw       [16]byte
	cmdKey    [16]byte
	authBlock cipher.Block
	client    string
}

// Validator 维护全部 VMess 账号并完成 AuthID → 账号 的认证（含时间窗与重放防护）。
type Validator struct {
	users  sync.Map
	replay *replayFilter
}

// NewValidator 创建空的账号验证器。
func NewValidator() *Validator {
	return &Validator{replay: newReplayFilter()}
}

// Add 注册一个账号；uuid 必须是标准 36 字符格式。
func (v *Validator) Add(uuid, client string) error {
	raw, err := parseUUID(uuid)
	if err != nil {
		return err
	}
	var cmdKey [16]byte
	copy(cmdKey[:], CmdKey(raw[:]))
	block, err := aes.NewCipher(KDF16(cmdKey[:], KDFSaltConstAuthIDEncryptionKey))
	if err != nil {
		return err
	}
	v.users.Store(uuid, &user{uuid: uuid, raw: raw, cmdKey: cmdKey, authBlock: block, client: client})
	return nil
}

// Remove 注销一个账号。
func (v *Validator) Remove(uuid string) {
	v.users.Delete(uuid)
}

// Reset 清空全部账号；外部账号表整体重灌前的第一步。
func (v *Validator) Reset() {
	v.users.Clear()
}

// Match 认证 AuthID：逐账号尝试。AuthID 由各账号独立密钥加密，无法从密文反查
// 归属，逐个解密不可避免（官方 AEAD 路径同构）；性能靠注册时预计算 AES 块——
// 内循环只剩一次块解密 + CRC，不再有 KDF 链与 key schedule。
func (v *Validator) Match(id authID) (*user, error) {
	now := time.Now()
	var matched *user
	v.users.Range(func(_, value any) bool {
		u := value.(*user)
		var plain [16]byte
		u.authBlock.Decrypt(plain[:], id[:])
		if _, ok := authPlainTime(&plain, now); ok {
			matched = u
			return false
		}
		return true
	})
	if matched == nil {
		return nil, errAuthIDUserNotFound
	}
	if !v.replay.Check(id) {
		return nil, errAuthIDReplay
	}
	return matched, nil
}

// ValidateUUID 校验 UUID 字符串格式（管理端录入前的把关）。
func ValidateUUID(uuid string) error {
	_, err := parseUUID(uuid)
	return err
}

// ParseUUID 解析标准 36 字符 UUID 为 16 字节。vless 与 vmess 共用同一凭据格式
// （uuid 用户同时可走两种入站），解析规则必须一致，故由 vmess 提供唯一实现。
func ParseUUID(s string) ([16]byte, error) {
	return parseUUID(s)
}
