package vmess

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hhy50/vnray/internal/identity"
)

const testUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"

// echoHandler 回显解密后的请求体（单元测试用）。
func echoHandler(id identity.Identity, target Target, conn *Conn) {
	_, _ = io.Copy(conn, conn)
	_ = conn.Close()
}

// startTestServer 起一个注册了 testUUID 的回显服务，返回监听地址。
func startTestServer(t *testing.T, uuid string) string {
	t.Helper()
	v := NewValidator()
	if err := v.Add(uuid, "client-a"); err != nil {
		t.Fatalf("add user: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := NewServer(v, echoHandler)
	go func() { _ = s.Serve(ln) }()
	return ln.Addr().String()
}

func TestAuthIDRoundtrip(t *testing.T) {
	raw, err := parseUUID(testUUID)
	if err != nil {
		t.Fatalf("parse uuid: %v", err)
	}
	cmdKey := CmdKey(raw[:])
	id := newAuthID(cmdKey[:], time.Now())
	if _, err := decodeAuthID(cmdKey[:], id); err != nil {
		t.Fatalf("decode own auth id: %v", err)
	}

	other := CmdKey([]byte("another-uuid-00000000"))
	if _, err := decodeAuthID(other[:], id); !errors.Is(err, errAuthIDInvalidTime) && err == nil {
		t.Fatalf("decode with wrong cmdKey should fail time/crc validation, got nil")
	}

	stale := newAuthID(cmdKey[:], time.Now().Add(-10*time.Minute))
	if _, err := decodeAuthID(cmdKey[:], stale); !errors.Is(err, errAuthIDInvalidTime) {
		t.Fatalf("stale auth id err = %v, want errAuthIDInvalidTime", err)
	}
}

func TestAEADHeaderRoundtrip(t *testing.T) {
	raw, _ := parseUUID(testUUID)
	cmdKey := CmdKey(raw[:])
	id := newAuthID(cmdKey[:], time.Now())
	plain := []byte("hello vmess header payload 0123456789")

	sealed := sealAEADHeader(cmdKey, id, plain)
	got, err := openAEADHeader(cmdKey[:], id, bytes.NewReader(sealed[16:]))
	if err != nil {
		t.Fatalf("open aead header: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip mismatch: %q", got)
	}

	if _, err := openAEADHeader(CmdKey([]byte("wrong-key-000000"))[:], id, bytes.NewReader(sealed)); err == nil {
		t.Fatal("open with wrong cmdKey should fail")
	}
}

func TestRequestRoundtrip(t *testing.T) {
	plain := make([]byte, 0, 64)
	plain = append(plain, 0x01)
	plain = append(plain, make([]byte, 16)...)                  // bodyIV
	plain = append(plain, make([]byte, 16)...)                  // bodyKey
	plain = append(plain, 0xAB)                                 // respV
	plain = append(plain, OptionChunkStream|OptionChunkMasking) // option
	plain = append(plain, SecurityNone)                         // padding(0)<<4 | security
	plain = append(plain, 0x00)                                 // reserved
	plain = append(plain, RequestCommandTCP)                    // command
	plain = append(plain, 0x1F, 0x90)                           // port 8080
	plain = append(plain, 0x02, byte(len("example.com")))
	plain = append(plain, "example.com"...)
	plain = append(plain, 0x00, 0x00, 0x00, 0x00) // checksum 占位
	fnv1a := fnvNew32a()
	_, _ = fnv1a.Write(plain[:len(plain)-4])
	sum := fnv1a.Sum32()
	copy(plain[len(plain)-4:], []byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)})

	req, err := parseRequest(plain)
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	if req.Target.Host != "example.com" || req.Target.Port != 8080 {
		t.Fatalf("target = %s, want example.com:8080", req.Target)
	}
	if req.Security != SecurityNone || req.Option&OptionChunkStream == 0 {
		t.Fatalf("security/option = %d/%d", req.Security, req.Option)
	}

	plain[len(plain)-1] ^= 0xFF
	if _, err := parseRequest(plain); err == nil {
		t.Fatal("bad checksum should fail")
	}
}

// TestVMessEcho 全链路回显：测试客户端按规范编码 → 入站服务解码 → 回显。
func TestVMessEcho(t *testing.T) {
	addr := startTestServer(t, testUUID)

	cases := []struct {
		name       string
		security   byte
		option     byte
		wantReject bool
	}{
		{"gcm+mask", SecurityAES128GCM, OptionChunkStream | OptionChunkMasking, false},
		{"gcm+mask+padding", SecurityAES128GCM, OptionChunkStream | OptionChunkMasking | OptionGlobalPadding, false},
		{"chacha+mask", SecurityChaCha20Poly1305, OptionChunkStream | OptionChunkMasking, false},
		{"none+chunk", SecurityNone, OptionChunkStream, false},
		{"authlen rejected", SecurityChaCha20Poly1305, OptionChunkStream | OptionAuthenticatedLength, true},
	}

	payload := bytes.Repeat([]byte("vnray-vmess-echo-payload-"), 1000)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			sess, err := NewClientSession(testUUID, conn, Target{Host: "example.com", Port: 443},
				tc.security, tc.option)
			if err != nil {
				t.Fatalf("new client session: %v", err)
			}
			if err := sess.ReadResponseHeader(); err != nil {
				if tc.wantReject {
					return // 服务器拒绝该选项组合并关闭连接，符合预期
				}
				t.Fatalf("read response header: %v", err)
			}
			if tc.wantReject {
				t.Fatal("authlen should have been rejected")
			}

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := sess.BodyWriter()
				if _, err := w.Write(payload); err != nil {
					t.Errorf("write body: %v", err)
				}
				if err := w.Close(); err != nil {
					t.Errorf("close body: %v", err)
				}
			}()

			got, err := io.ReadAll(sess.BodyReader())
			wg.Wait()
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("echo mismatch: got %d bytes", len(got))
			}
		})
	}
}

// TestVMessAuthFailures 未注册 uuid 与重放防护。
func TestVMessAuthFailures(t *testing.T) {
	addr := startTestServer(t, testUUID)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	sess, err := NewClientSession("00000000-0000-0000-0000-000000000000", conn,
		Target{Host: "example.com", Port: 443}, SecurityAES128GCM, OptionChunkStream)
	if err != nil {
		t.Fatalf("new client session: %v", err)
	}
	if err := sess.ReadResponseHeader(); err == nil {
		t.Fatal("unregistered uuid should be rejected")
	}
	_ = conn.Close()
}
