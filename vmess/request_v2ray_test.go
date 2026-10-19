package vmess

// 对照 v2fly/Xray 真实客户端字节的回归测试：mux 请求头里**没有地址段**。
// v2fly encoding/client.go 的 EncodeRequestHeader 只在 Command != Mux 时写
// WriteAddressPort，服务端 encoding/server.go 遇到 Command == Mux 直接把
// 目标合成 v1.mux.cool:0。这里按同一字节布局构造请求，验证解析侧不能去读地址。

import (
	"hash/fnv"
	"testing"
)

// v2rayMuxPlain 按 v2fly 客户端布局构造 mux 请求明文：38B 固定段 + padding + 4B 校验。
func v2rayMuxPlain(padding int) []byte {
	plain := make([]byte, 38)
	plain[0] = 0x01
	for i := 1; i < 33; i++ {
		plain[i] = byte(i)
	}
	plain[33] = 0x7f
	plain[34] = OptionChunkStream | OptionChunkMasking
	plain[35] = byte(padding<<4) | SecurityAES128GCM
	plain[37] = RequestCommandMux
	for i := 0; i < padding; i++ {
		plain = append(plain, byte(0xa0+i))
	}
	fnv1a := fnv.New32a()
	_, _ = fnv1a.Write(plain)
	sum := fnv1a.Sum32()
	return append(plain, byte(sum>>24), byte(sum>>16), byte(sum>>8), byte(sum))
}

// TestParseRequestV2RayMuxHeader 覆盖 v2ray 客户端的 mux 请求头：填充长度随机
// （0..15），解析必须成功且目标被识别为 v1.mux.cool。
func TestParseRequestV2RayMuxHeader(t *testing.T) {
	for padding := 0; padding <= 15; padding++ {
		req, err := parseRequest(v2rayMuxPlain(padding))
		if err != nil {
			t.Fatalf("padding=%d: parseRequest failed: %v", padding, err)
		}
		if !req.IsMux() {
			t.Fatalf("padding=%d: request not recognized as mux, target=%s", padding, req.Target)
		}
		if req.Target.Host != MuxCoolHost {
			t.Fatalf("padding=%d: target host = %q, want %q", padding, req.Target.Host, MuxCoolHost)
		}
		if err := req.validateOptions(); err != nil {
			t.Fatalf("padding=%d: validateOptions: %v", padding, err)
		}
	}
}

// TestParseRequestTCPStillParsesAddress 保证普通 TCP 请求仍按端口/地址段解析。
func TestParseRequestTCPStillParsesAddress(t *testing.T) {
	plain := make([]byte, 38)
	plain[0] = 0x01
	plain[34] = OptionChunkStream | OptionChunkMasking
	plain[35] = SecurityAES128GCM
	plain[37] = RequestCommandTCP
	plain = append(plain, 0x01, 0xbb) // port 443
	plain = append(plain, 0x02, byte(len("example.com")))
	plain = append(plain, "example.com"...)

	fnv1a := fnv.New32a()
	_, _ = fnv1a.Write(plain)
	sum := fnv1a.Sum32()
	plain = append(plain, byte(sum>>24), byte(sum>>16), byte(sum>>8), byte(sum))

	req, err := parseRequest(plain)
	if err != nil {
		t.Fatalf("parseRequest: %v", err)
	}
	if req.Target.Host != "example.com" || req.Target.Port != 443 {
		t.Fatalf("target = %s, want example.com:443", req.Target)
	}
	if req.IsMux() {
		t.Fatal("tcp request wrongly recognized as mux")
	}
}
