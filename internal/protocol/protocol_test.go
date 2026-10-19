package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	addrPayload, err := Address{Host: "127.0.0.1", Port: 9001}.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("hello binary protocol")
	out := &bytes.Buffer{}
	enc := NewEncoder(out)
	in := NewBufferedFrame(Forward, append(addrPayload, body...))
	if err := enc.WriteFrame(in); err != nil {
		t.Fatal(err)
	}

	dec := NewDecoder(out)
	frame, err := dec.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != Forward {
		t.Fatalf("unexpected header: %+v", frame)
	}
	if frame.PayloadLen != uint32(len(addrPayload)+len(body)) {
		t.Fatalf("unexpected payload length: %+v", frame)
	}
	addr, err := ReadAddress(frame.Body)
	if err != nil {
		t.Fatal(err)
	}
	if addr.Host != "127.0.0.1" || addr.Port != 9001 {
		t.Fatalf("address = %+v", addr)
	}
	got, err := io.ReadAll(frame.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("payload = %q", got)
	}
	if err := frame.CloseBody(); err != nil {
		t.Fatalf("crc verify: %v", err)
	}
}

// TestWireLayout 把线上布局钉死：偏移写错、字段宽度回退都会在这里失败。
func TestWireLayout(t *testing.T) {
	// Forward 帧：固定头 10 + 地址块(1+1+2) + 载荷 1 + CRC 4 = 19。
	buf := &bytes.Buffer{}
	addr, err := Address{Host: "h", Port: 9001}.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := NewEncoder(buf).WriteFrame(NewBufferedFrame(Forward, append(addr, 'x'))); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if len(b) != 19 {
		t.Fatalf("帧长 = %d, want 19", len(b))
	}
	if b[0] != 0x20 || b[1] != 0x26 {
		t.Fatalf("魔数 = %02x%02x, want 2026", b[0], b[1])
	}
	if got := uint16(b[2])<<8 | uint16(b[3]); got != uint16(Forward) {
		t.Fatalf("类型 = %d, want %d", got, Forward)
	}
	if b[4] != 0 || b[5] != 0 || b[6] != 0 || b[7] != 0 {
		t.Fatalf("标志与保留字节 = %v, want 全零", b[4:8])
	}
	if got := uint16(b[8])<<8 | uint16(b[9]); got != 5 {
		t.Fatalf("载荷长度 = %d, want 5", got)
	}
	// 载荷：地址块 hostLen(1)+host('h')+port(9001)，其后是载荷 'x'。
	if b[10] != 1 || b[11] != 'h' {
		t.Fatalf("地址块 hostLen/host = %d/%q", b[10], b[11])
	}
	if got := uint16(b[12])<<8 | uint16(b[13]); got != 9001 {
		t.Fatalf("端口 = %d, want 9001", got)
	}
	if b[14] != 'x' {
		t.Fatalf("载荷 = %q", b[14])
	}

	// 无目标、无载荷的最小帧：固定头 10 + CRC 4 = 14。
	buf.Reset()
	if err := NewEncoder(buf).WriteFrame(NewBufferedFrame(ForwardResult, nil)); err != nil {
		t.Fatal(err)
	}
	if got := buf.Len(); got != headerLen+crcLen {
		t.Fatalf("最小帧长 = %d, want %d", got, headerLen+crcLen)
	}
}

// TestForwardTunnelFrames 断言 Forward 载荷即地址块、Tunnel 载荷 = 地址块 + JSON，
// 且 ReadTunnelRequest 能原样读回。
func TestForwardTunnelFrames(t *testing.T) {
	const host = "example.com"
	const port = 8443
	a := Address{Host: host, Port: port}

	buf := &bytes.Buffer{}
	fwd, err := NewForwardFrame(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewEncoder(buf).WriteFrame(fwd); err != nil {
		t.Fatal(err)
	}
	frame, err := NewDecoder(buf).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	gotAddr, err := ReadAddress(frame.Body)
	if err != nil {
		t.Fatal(err)
	}
	if gotAddr != a {
		t.Fatalf("forward address = %+v", gotAddr)
	}
	if err := frame.CloseBody(); err != nil {
		t.Fatalf("crc: %v", err)
	}

	buf.Reset()
	tun, err := NewTunnelFrame(TunnelRequest{Origin: "10.0.0.1:5555"}, a)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewEncoder(buf).WriteFrame(tun); err != nil {
		t.Fatal(err)
	}
	frame, err = NewDecoder(buf).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	gotAddr, req, err := ReadTunnelRequest(frame.Body)
	if err != nil {
		t.Fatal(err)
	}
	if gotAddr != a || req.Origin != "10.0.0.1:5555" {
		t.Fatalf("tunnel = %+v %+v", gotAddr, req)
	}
	if err := frame.CloseBody(); err != nil {
		t.Fatalf("crc: %v", err)
	}
}

// TestHeartbeatPayload 断言时间戳走心跳载荷：8 字节大端，往返一致，长度不符即报协议错。
func TestHeartbeatPayload(t *testing.T) {
	const sent = 1_700_000_000_123
	buf := &bytes.Buffer{}
	if err := NewEncoder(buf).WriteFrame(NewHeartbeatFrame(Ping, sent)); err != nil {
		t.Fatal(err)
	}
	if got := buf.Len(); got != headerLen+heartbeatPayloadLen+crcLen {
		t.Fatalf("心跳帧长 = %d, want %d", got, headerLen+heartbeatPayloadLen+crcLen)
	}

	frame, err := NewDecoder(buf).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != Ping || frame.PayloadLen != heartbeatPayloadLen {
		t.Fatalf("心跳帧 = %+v", frame)
	}
	ts, err := ReadHeartbeatTimestamp(frame)
	if err != nil {
		t.Fatal(err)
	}
	if ts != sent {
		t.Fatalf("时间戳回读 = %d, want %d", ts, sent)
	}

	bad := NewBufferedFrame(Ping, []byte("short"))
	if _, err := ReadHeartbeatTimestamp(bad); !errors.Is(err, ErrProtocol) {
		t.Fatalf("want protocol error, got %v", err)
	}
}

// TestPushConfigRoundTrip 断言 ConfigPush 帧载荷（PushConfig JSON）能完整往返。
func TestPushConfigRoundTrip(t *testing.T) {
	cfg := PushConfig{
		CustomHosts: []CustomHost{
			{Domain: "mirror.local", IP: "10.0.0.8"},
			{Domain: "api.internal", IP: "192.168.1.2"},
		},
		IPMaps: []IPMap{
			{From: "123.123.*.*", To: "192.168.*.*"},
		},
	}
	buf := &bytes.Buffer{}
	pushFrame, err := NewPushConfigFrame(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewEncoder(buf).WriteFrame(pushFrame); err != nil {
		t.Fatal(err)
	}

	got, rerr := NewDecoder(buf).ReadFrame()
	if rerr != nil {
		t.Fatal(rerr)
	}
	if got.Type != ConfigPush {
		t.Fatalf("类型 = %d, want %d", got.Type, ConfigPush)
	}
	payload, err := ReadPayload(got)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	parsed, err := ParsePushConfig(payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.CustomHosts) != 2 || parsed.CustomHosts[0] != cfg.CustomHosts[0] {
		t.Fatalf("parsed = %+v, want %+v", parsed, cfg)
	}
	if len(parsed.IPMaps) != 1 || parsed.IPMaps[0] != cfg.IPMaps[0] {
		t.Fatalf("parsed ip_maps = %+v, want %+v", parsed.IPMaps, cfg.IPMaps)
	}

	// 认证应答携带同一快照：Reply 的 config 字段 JSON 往返一致。
	b, err := json.Marshal(Reply{OK: true, Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	var res Reply
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if res.Config == nil || len(res.Config.CustomHosts) != 2 || len(res.Config.IPMaps) != 1 {
		t.Fatalf("reply config = %+v", res.Config)
	}
}

// TestFieldWidthLimits 断言收窄过的字段在超限时报协议错，而不是静默截断。
func TestFieldWidthLimits(t *testing.T) {
	t.Run("host 超过 255 字节", func(t *testing.T) {
		a := Address{Host: strings.Repeat("h", math.MaxUint8+1), Port: 1}
		if _, err := a.Bytes(); !errors.Is(err, ErrProtocol) {
			t.Fatalf("want protocol error, got %v", err)
		}
		if _, err := NewForwardFrame(a); !errors.Is(err, ErrProtocol) {
			t.Fatalf("want protocol error, got %v", err)
		}
	})

	t.Run("单帧载荷超过 65535 字节", func(t *testing.T) {
		payload := make([]byte, math.MaxUint16+1)
		err := NewEncoder(io.Discard).WriteFrame(NewBufferedFrame(Forward, payload))
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("want protocol error, got %v", err)
		}
	})
}

func TestDecodeErrors(t *testing.T) {
	t.Run("clean EOF", func(t *testing.T) {
		if _, err := NewDecoder(bytes.NewBuffer(nil)).ReadFrame(); err != io.EOF {
			t.Fatalf("want io.EOF, got %v", err)
		}
	})

	t.Run("bad magic", func(t *testing.T) {
		buf := &bytes.Buffer{}
		frame := NewBufferedFrame(Ping, []byte("x"))
		if err := NewEncoder(buf).WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
		buf.Bytes()[0] = 'X' // 破坏魔数
		if _, err := NewDecoder(buf).ReadFrame(); !errors.Is(err, ErrProtocol) {
			t.Fatalf("want protocol error, got %v", err)
		}
	})

	t.Run("crc mismatch", func(t *testing.T) {
		buf := &bytes.Buffer{}
		if err := NewEncoder(buf).WriteFrame(NewBufferedFrame(Ping, []byte("xyz"))); err != nil {
			t.Fatal(err)
		}
		b := buf.Bytes()
		b[len(b)-crcLen-1] ^= 0xFF // 在 CRC 之前翻转一个载荷字节
		dec := NewDecoder(buf)
		frame, err := dec.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, frame.Body); err != nil {
			t.Fatal(err)
		}
		if err := frame.CloseBody(); !errors.Is(err, ErrProtocol) {
			t.Fatalf("want crc protocol error, got %v", err)
		}
	})

	t.Run("unknown type", func(t *testing.T) {
		buf := &bytes.Buffer{}
		// 用明确的越界值而不是 255：类型是 2 字节，255 将来可能真被占用。
		bad := NewBufferedFrame(Type(0xFFFF), nil)
		if err := NewEncoder(buf).WriteFrame(bad); err == nil {
			t.Fatal("encoder must reject unknown type")
		}
	})
}
