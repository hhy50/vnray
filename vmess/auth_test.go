package vmess

// 认证热路径与会话判重的测试：Validator 预计算后的匹配正确性、sessionHistory
// 的判重语义，以及服务端对同指纹重放的拒绝；基准对照新旧认证内循环成本。

import (
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/identity"
)

// numberedUUID 生成第 i 个合法测试 UUID（避开 vmess_test.go 的 testUUID 常量）。
func numberedUUID(i int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012x", i)
}

// mustRaw 解析 UUID 为 16 字节，失败即 Fail（测试辅助）。
func mustRaw(t testing.TB, uuid string) [16]byte {
	t.Helper()
	raw, err := parseUUID(uuid)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", uuid, err)
	}
	return raw
}

// fixedMaterial 返回一组固定的请求体密钥与 IV。
func fixedMaterial() (bodyKey, bodyIV [16]byte) {
	for i := range bodyKey {
		bodyKey[i] = byte(i + 1)
		bodyIV[i] = byte(255 - i)
	}
	return bodyKey, bodyIV
}

// TestValidatorMatchPrecomputed 覆盖预计算后的匹配：命中、未命中、过期与重放。
func TestValidatorMatchPrecomputed(t *testing.T) {
	v := NewValidator()
	for i := 0; i < 3; i++ {
		if err := v.Add(numberedUUID(i), fmt.Sprintf("client-%d", i)); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	for i := 0; i < 3; i++ {
		raw := mustRaw(t, numberedUUID(i))
		id := newAuthID(CmdKey(raw[:]), time.Now())
		u, err := v.Match(id)
		if err != nil {
			t.Fatalf("user %d: match: %v", i, err)
		}
		if u.uuid != numberedUUID(i) || u.client != fmt.Sprintf("client-%d", i) {
			t.Fatalf("user %d: matched %s/%s", i, u.uuid, u.client)
		}
	}

	// 过期 AuthID 不认。
	raw0 := mustRaw(t, numberedUUID(0))
	stale := newAuthID(CmdKey(raw0[:]), time.Now().Add(-5*time.Minute))
	if _, err := v.Match(stale); !errors.Is(err, errAuthIDUserNotFound) {
		t.Fatalf("stale auth id: got %v, want not found", err)
	}

	// 同一 AuthID 第二次出现判重放。
	raw1 := mustRaw(t, numberedUUID(1))
	fresh := newAuthID(CmdKey(raw1[:]), time.Now())
	if _, err := v.Match(fresh); err != nil {
		t.Fatalf("fresh auth id: %v", err)
	}
	if _, err := v.Match(fresh); !errors.Is(err, errAuthIDReplay) {
		t.Fatalf("replayed auth id: got %v, want replay", err)
	}
}

// TestSessionHistory 覆盖判重表语义：重复拒绝、不同指纹放行、过期放行。
func TestSessionHistory(t *testing.T) {
	h := newSessionHistory()
	bodyKey, bodyIV := fixedMaterial()
	u0 := mustRaw(t, numberedUUID(0))
	u1 := mustRaw(t, numberedUUID(1))

	if !h.Check(u0, bodyKey, bodyIV) {
		t.Fatal("first request should pass")
	}
	if h.Check(u0, bodyKey, bodyIV) {
		t.Fatal("same fingerprint within TTL should be rejected")
	}
	if !h.Check(u1, bodyKey, bodyIV) {
		t.Fatal("different user with same key/iv should pass")
	}
	if h.Check(u0, bodyKey, bodyIV) {
		t.Fatal("same fingerprint should stay rejected")
	}

	// 过期条目视为新请求（v2fly 语义：覆盖时间戳并放行）。
	var key sessionKey
	copy(key[0:16], u0[:])
	copy(key[16:32], bodyKey[:])
	copy(key[32:48], bodyIV[:])
	h.seen[key] = time.Now().Add(-4 * time.Minute)
	if !h.Check(u0, bodyKey, bodyIV) {
		t.Fatal("expired fingerprint should pass")
	}
	if h.Check(u0, bodyKey, bodyIV) {
		t.Fatal("re-registered fingerprint should be rejected again")
	}
}

// dialVMessRequest 以固定请求体材料向 VMess 服务端发一条完整握手。
func dialVMessRequest(t testing.TB, addr, uuid string, bodyKey, bodyIV [16]byte) net.Conn {
	t.Helper()
	raw := mustRaw(t, uuid)
	cmdKey := CmdKey(raw[:])

	plain := make([]byte, 38)
	plain[0] = 0x01
	copy(plain[1:17], bodyIV[:])
	copy(plain[17:33], bodyKey[:])
	plain[33] = 0x42
	plain[34] = OptionChunkStream | OptionChunkMasking
	plain[35] = SecurityAES128GCM
	plain[37] = RequestCommandTCP
	plain = append(plain, 0x01, 0xbb) // 443
	plain = append(plain, 0x02, byte(len("example.com")))
	plain = append(plain, "example.com"...)
	fnv1a := fnvNew32a()
	_, _ = fnv1a.Write(plain)
	sum := fnv1a.Sum32()
	plain = append(plain, byte(sum>>24), byte(sum>>16), byte(sum>>8), byte(sum))

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	id := newAuthID(cmdKey, time.Now())
	if _, err := conn.Write(sealAEADHeader(cmdKey, id, plain)); err != nil {
		t.Fatalf("write header: %v", err)
	}
	return conn
}

// TestServerRejectsDuplicatedSession 走真实服务端路径：同指纹换 AuthID 的
// 第二条连接必须被拒绝（无响应头、连接关闭），首条正常拿到响应头。
func TestServerRejectsDuplicatedSession(t *testing.T) {
	uuid := numberedUUID(7)
	validator := NewValidator()
	if err := validator.Add(uuid, "client-a"); err != nil {
		t.Fatalf("add: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	var served atomic.Int32
	server := NewServer(validator, func(id identity.Identity, target Target, conn *Conn) {
		served.Add(1)
		_ = conn.Close()
	})
	go func() { _ = server.Serve(ln) }()

	bodyKey, bodyIV := fixedMaterial()
	addr := ln.Addr().String()

	conn1 := dialVMessRequest(t, addr, uuid, bodyKey, bodyIV)
	_ = conn1.SetReadDeadline(time.Now().Add(2 * time.Second))
	var lenBlock [18]byte
	if _, err := readFull(conn1, lenBlock[:]); err != nil {
		t.Fatalf("first connection should receive response header, got %v", err)
	}

	conn2 := dialVMessRequest(t, addr, uuid, bodyKey, bodyIV)
	_ = conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := readFull(conn2, lenBlock[:]); err == nil {
		t.Fatal("second connection with same fingerprint should be rejected")
	}
	if served.Load() != 1 {
		t.Fatalf("handler invoked %d times, want 1", served.Load())
	}
}

// readFull 读完整个缓冲（io.ReadFull 语义），测试辅助。
func readFull(c net.Conn, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := c.Read(buf[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

// BenchmarkValidatorMatchScan200 测量 200 个账号下未命中路径的单次认证成本
// （内循环 = 200 次 AES 块解密 + CRC，无 KDF/key schedule）。
func BenchmarkValidatorMatchScan200(b *testing.B) {
	v := NewValidator()
	for i := 0; i < 200; i++ {
		if err := v.Add(numberedUUID(i), "c"); err != nil {
			b.Fatalf("add: %v", err)
		}
	}
	probe := mustRaw(b, numberedUUID(999))
	id := newAuthID(CmdKey(probe[:]), time.Now())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = v.Match(id)
	}
}

// BenchmarkAuthAttemptLegacy200 对照旧实现：200 个账号逐个重做 KDF 派生与
// key schedule（即重构前 Match 的每连接成本）。
func BenchmarkAuthAttemptLegacy200(b *testing.B) {
	keys := make([][]byte, 200)
	for i := range keys {
		raw := mustRaw(b, numberedUUID(i))
		keys[i] = CmdKey(raw[:])
	}
	id := newAuthID(keys[0][:], time.Now())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, k := range keys {
			_, _ = decodeAuthID(k[:], id)
		}
	}
}
