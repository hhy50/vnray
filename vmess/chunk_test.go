package vmess

import (
	"bytes"
	"io"
	"testing"
)

// TestChunkRoundtrip 全组合验证分块框架：内存管道上写入→读取逐字节一致。
func TestChunkRoundtrip(t *testing.T) {
	securityCases := []struct {
		name     string
		security byte
	}{
		{"gcm", SecurityAES128GCM},
		{"chacha", SecurityChaCha20Poly1305},
		{"none", SecurityNone},
	}
	optionCases := []struct {
		name   string
		option byte
	}{
		{"chunk", OptionChunkStream},
		{"chunk+mask", OptionChunkStream | OptionChunkMasking},
		{"chunk+mask+padding", OptionChunkStream | OptionChunkMasking | OptionGlobalPadding},
	}
	payloads := [][]byte{
		bytes.Repeat([]byte("a"), 100),
		bytes.Repeat([]byte("b"), maxChunkPayload+123), // 跨块
		[]byte("x"),
	}

	for _, sc := range securityCases {
		for _, oc := range optionCases {
			for i, payload := range payloads {
				name := sc.name + "/" + oc.name
				t.Run(name, func(t *testing.T) {
					req := &Request{Security: sc.security, Option: oc.option, bodyKey: [16]byte{1}, bodyIV: [16]byte{2}}
					if err := req.validateOptions(); err != nil {
						t.Skipf("invalid combo: %v", err)
					}
					wAuth, wCodec, wPadding, err := chunkCodec(req, false)
					if err != nil {
						t.Fatalf("write codec: %v", err)
					}
					rAuth, rCodec, rPadding, err := chunkCodec(req, false)
					if err != nil {
						t.Fatalf("read codec: %v", err)
					}

					var wire bytes.Buffer
					w := newChunkWriter(&wire, wAuth, wCodec, wPadding)
					if _, err := w.Write(payload); err != nil {
						t.Fatalf("write: %v", err)
					}
					if err := w.Close(); err != nil {
						t.Fatalf("close: %v", err)
					}

					r := newChunkReader(bytes.NewReader(wire.Bytes()), rAuth, rCodec, rPadding)
					got, err := io.ReadAll(r)
					if err != nil {
						t.Fatalf("read: %v", err)
					}
					if !bytes.Equal(got, payload) {
						t.Fatalf("roundtrip mismatch: got %d bytes, want %d", len(got), len(payload))
					}
					_ = i
				})
			}
		}
	}
}
