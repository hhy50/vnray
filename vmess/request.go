package vmess

// 请求/响应头的解析与构造。

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
)

// Target 是 VMess 请求的目标地址。
type Target struct {
	Host string
	Port int
}

// String 实现 fmt.Stringer。
func (t Target) String() string { return fmt.Sprintf("%s:%d", t.Host, t.Port) }

// Request 是解析完成的 VMess 请求头。
type Request struct {
	// Target 是请求目标（帧头里的端口与地址）。mux 主连接的目标是标记域名
	// v1.mux.cool，真实目标在各 mux 子连接帧里。
	Target Target
	// Security 是载荷加密方式（AES128GCM / ChaCha20Poly1305 / None）。
	Security byte
	// Option 是请求头里的选项位，响应侧按同样的框架编码。
	Option byte
	// Command 是请求命令：TCP 直达目标，Mux 表示 mux 主连接。
	Command byte

	respV   byte // 响应头首字节必须回显请求头 33 号字节
	bodyKey [16]byte
	bodyIV  [16]byte
}

// IsMux 判断该请求是否是 mux 主连接：命令为 Mux，或目标为标记域名
// v1.mux.cool（v2fly/Xray 服务端即按目标域名识别，命令由客户端从目标推导）。
func (r *Request) IsMux() bool {
	return r.Command == RequestCommandMux || r.Target.Host == MuxCoolHost
}

var errBadSecurity = errors.New("vmess: unsupported security")

// parseRequest 解析 AEAD 头解密出的明文：38B 固定段 + [端口/地址] + 填充 + 4B FNV 校验。
// 端口/地址段只存在于 TCP 请求；mux 请求头里没有地址，目标由命令反推（见下）。
func parseRequest(plain []byte) (*Request, error) {
	if len(plain) < 38+4 {
		return nil, errors.New("vmess: request header too short")
	}
	fnv1a := fnv.New32a()
	_, _ = fnv1a.Write(plain[:len(plain)-4])
	if actual := fnv1a.Sum32(); actual != binary.BigEndian.Uint32(plain[len(plain)-4:]) {
		return nil, errors.New("vmess: request checksum mismatch")
	}

	r := &Request{}
	copy(r.bodyIV[:], plain[1:17])
	copy(r.bodyKey[:], plain[17:33])
	r.respV = plain[33]
	r.Option = plain[34]
	paddingLen := int(plain[35] >> 4)
	security := plain[35] & 0x0F
	command := plain[37]

	switch security {
	case SecurityAES128GCM, SecurityChaCha20Poly1305, SecurityNone:
		r.Security = security
	default:
		return nil, fmt.Errorf("%w: %d", errBadSecurity, security)
	}
	switch command {
	case RequestCommandTCP, RequestCommandUDP, RequestCommandMux:
		r.Command = command
	default:
		return nil, fmt.Errorf("vmess: unsupported command %d (tcp, udp or mux only)", command)
	}

	// 地址段：端口 2B → 类型 1B（1=IPv4 / 2=域名 / 3=IPv6）→ 地址本体。
	// mux 请求头不带地址段——v2fly 客户端只在 Command != Mux 时才写地址，
	// 服务端直接按命令合成标记目标；这里同样反推，不能去读那两字节。
	pos := 38
	if command == RequestCommandMux {
		r.Target = Target{Host: MuxCoolHost, Port: 0}
	} else {
		var err error
		if pos, err = r.parseTarget(plain, pos); err != nil {
			return nil, err
		}
	}
	pos += paddingLen
	if pos+4 > len(plain) {
		return nil, errors.New("vmess: truncated request checksum")
	}
	r.Option &= OptionChunkStream | OptionChunkMasking | OptionGlobalPadding | OptionAuthenticatedLength
	return r, nil
}

// parseTarget 解析端口与地址段，返回地址段结束位置。
func (r *Request) parseTarget(plain []byte, pos int) (int, error) {
	if len(plain) < pos+2+1+4 {
		return 0, errors.New("vmess: truncated request address")
	}
	r.Target.Port = int(binary.BigEndian.Uint16(plain[pos : pos+2]))
	pos += 2
	switch plain[pos] {
	case 1:
		if len(plain) < pos+1+4+4 {
			return 0, errors.New("vmess: truncated ipv4 address")
		}
		r.Target.Host = ipString(plain[pos+1 : pos+5])
		pos += 5
	case 2:
		n := int(plain[pos+1])
		if len(plain) < pos+2+n+4 {
			return 0, errors.New("vmess: truncated domain address")
		}
		r.Target.Host = string(plain[pos+2 : pos+2+n])
		pos += 2 + n
	case 3:
		if len(plain) < pos+1+16+4 {
			return 0, errors.New("vmess: truncated ipv6 address")
		}
		r.Target.Host = ipString(plain[pos+1 : pos+17])
		pos += 17
	default:
		return 0, fmt.Errorf("vmess: unknown address type %d", plain[pos])
	}
	return pos, nil
}

// validateOptions 校验选项组合：GlobalPadding 依赖 shake 掩码；
// AuthenticatedLength（长度字段整体 AEAD）暂不支持，握手即拒；
// mux 主连接必须有分块框架——mux 帧的长度语义完全建立在 chunk stream 之上。
func (r *Request) validateOptions() error {
	if r.Security == SecurityNone && r.Option&OptionChunkStream == 0 {
		return errors.New("vmess: security none requires chunk stream")
	}
	if r.Command == RequestCommandMux && r.Option&OptionChunkStream == 0 {
		return errors.New("vmess: mux requires chunk stream")
	}
	if r.Option&OptionAuthenticatedLength != 0 {
		return errors.New("vmess: authenticated length is not supported")
	}
	if r.Option&OptionGlobalPadding != 0 && r.Option&OptionChunkMasking == 0 {
		return errors.New("vmess: global padding requires chunk masking")
	}
	return nil
}

// buildResponseHeader 构造响应头（4B 明文 [V, option, 0, 0] 经两级 AEAD），
// 响应体密钥由请求密钥 SHA256 派生。
func buildResponseHeader(req *Request) (respHdr []byte, respKey [16]byte, respIV [16]byte) {
	respKey = sha256Sum16(req.bodyKey[:])
	respIV = sha256Sum16(req.bodyIV[:])

	plain := []byte{req.respV, 0x00, 0x00, 0x00}

	// 长度块封装的是 2B 长度值，载荷块封装的才是 4B 明文头。
	lenPlain := []byte{byte(len(plain) >> 8), byte(len(plain))}
	lenKey := KDF16(respKey[:], KDFSaltConstAEADRespHeaderLenKey)
	lenIV := KDF(respIV[:], KDFSaltConstAEADRespHeaderLenIV)[:12]
	lenBlock := newAESGCM(lenKey).Seal(nil, lenIV, lenPlain, nil)

	payloadKey := KDF16(respKey[:], KDFSaltConstAEADRespHeaderPayloadKey)
	payloadIV := KDF(respIV[:], KDFSaltConstAEADRespHeaderPayloadIV)[:12]
	payloadBlock := newAESGCM(payloadKey).Seal(nil, payloadIV, plain, nil)

	respHdr = append(respHdr, lenBlock...)
	respHdr = append(respHdr, payloadBlock...)
	return respHdr, respKey, respIV
}

// chunkCodec 按请求选项构造载荷分块编解码器。resp 标记当前是响应侧：
// 载荷密钥用响应派生对，而 authLen 长度加密的 nonce 流始终取请求 IV
// （与 xray 实现一致的自身不对称性）。
func chunkCodec(req *Request, resp bool) (auth *chunkAuth, codec sizeCodec, padding bool, err error) {
	bodyKey, bodyIV := req.bodyKey, req.bodyIV
	if resp {
		bodyKey = sha256Sum16(req.bodyKey[:])
		bodyIV = sha256Sum16(req.bodyIV[:])
	}

	switch req.Security {
	case SecurityAES128GCM:
		auth = newChunkAuth(newAESGCM(bodyKey[:]), bodyIV[:])
	case SecurityChaCha20Poly1305:
		auth = newChunkAuth(newChaCha(GenerateChacha20Poly1305Key(bodyKey[:])), bodyIV[:])
	default:
		auth = newChunkAuth(newNoOp(), bodyIV[:])
	}

	if req.Option&OptionChunkMasking != 0 {
		return auth, newShakeSizeCodec(bodyIV[:]), req.Option&OptionGlobalPadding != 0, nil
	}
	if req.Option&OptionGlobalPadding != 0 {
		return nil, nil, false, errors.New("vmess: global padding requires chunk masking")
	}
	return auth, plainSizeCodec{}, false, nil
}

// lengthAuthAEAD 构造 authLen 长度字段的认证器，加密套件跟随载荷安全级别。
func lengthAuthAEAD(bodyKey []byte, security byte) cipher.AEAD {
	key := KDF16(bodyKey, "auth_len")
	if security == SecurityChaCha20Poly1305 {
		return newChaCha(GenerateChacha20Poly1305Key(key))
	}
	return newAESGCM(key)
}
