package vmess

import (
	"bytes"
	"errors"
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

					source := bytes.NewReader(wire.Bytes())
					r := newChunkReader(source, rAuth, rCodec, rPadding)
					got, err := io.ReadAll(r)
					if err != nil {
						t.Fatalf("read: %v", err)
					}
					if !bytes.Equal(got, payload) {
						t.Fatalf("roundtrip mismatch: got %d bytes, want %d", len(got), len(payload))
					}
					if source.Len() != 0 {
						t.Fatalf("terminal chunk left %d unread bytes", source.Len())
					}
					_ = i
				})
			}
		}
	}
}

func TestChunkTerminalAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "valid"},
		{name: "corrupt tag", mutate: func(wire []byte) []byte { wire[len(wire)-1] ^= 1; return wire }},
		{name: "truncated tag", mutate: func(wire []byte) []byte { return wire[:len(wire)-1] }},
		{name: "short size", mutate: func(wire []byte) []byte { wire[1] = 1; return wire }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &Request{Security: SecurityAES128GCM, Option: OptionChunkStream, bodyKey: [16]byte{1}, bodyIV: [16]byte{2}}
			wa, wc, wp, err := chunkCodec(req, false)
			if err != nil {
				t.Fatal(err)
			}
			ra, rc, rp, err := chunkCodec(req, false)
			if err != nil {
				t.Fatal(err)
			}
			var wire bytes.Buffer
			if err := newChunkWriter(&wire, wa, wc, wp).Close(); err != nil {
				t.Fatal(err)
			}
			data := wire.Bytes()
			if tc.mutate != nil {
				data = tc.mutate(data)
			}
			_, err = newChunkReader(bytes.NewReader(data), ra, rc, rp).Read(make([]byte, 1))
			if tc.mutate == nil {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("terminal error = %v, want EOF", err)
				}
			} else if err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("invalid terminal chunk accepted: %v", err)
			}
		})
	}
}
