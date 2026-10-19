package vmess

// ClientSession 是最小 VMess 客户端会话编码器：集成测试与联调工具用，
// 与本包的入站服务互为镜像（字节格式同源同验）。

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"time"
)

// ClientSession 已拨号的 VMess 客户端会话：构造即发送请求头。
type ClientSession struct {
	conn     net.Conn
	security byte
	option   byte
	bodyIV   [16]byte
	bodyKey  [16]byte
	respV    byte
	respKey  [16]byte
	respIV   [16]byte
	bodyW    *chunkWriter
	bodyR    *chunkReader
}

// NewClientSession 在 conn 上完成 VMess 请求头发送（AEAD 认证 + 加密头），
// option/security 决定载荷框架，dest 为代理目标。
func NewClientSession(uuid string, conn net.Conn, dest Target, security, option byte) (*ClientSession, error) {
	return newClientSession(uuid, conn, dest, RequestCommandTCP, security, option)
}

// NewMuxClientSession 在 conn 上完成 mux 主连接的请求头发送：目标为标记域名
// v1.mux.cool、命令为 Mux（v2fly/Xray 客户端同值），此后 conn 的载荷通道承载
// mux.cool 帧，真实目标在各子连接帧里。
func NewMuxClientSession(uuid string, conn net.Conn, security, option byte) (*ClientSession, error) {
	return newClientSession(uuid, conn,
		Target{Host: MuxCoolHost, Port: MuxCoolPort}, RequestCommandMux, security, option)
}

// newClientSession 是两种命令共用的请求头构造路径。
func newClientSession(uuid string, conn net.Conn, dest Target, command, security, option byte) (*ClientSession, error) {
	raw, err := parseUUID(uuid)
	if err != nil {
		return nil, err
	}
	cmdKey := CmdKey(raw[:])

	var bodyIV, bodyKey [16]byte
	if _, err := rand.Read(bodyIV[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(bodyKey[:]); err != nil {
		return nil, err
	}
	var respV [1]byte
	if _, err := rand.Read(respV[:]); err != nil {
		return nil, err
	}

	plain := make([]byte, 0, 38)
	plain = append(plain, 0x01) // version
	plain = append(plain, bodyIV[:]...)
	plain = append(plain, bodyKey[:]...)
	plain = append(plain, respV[0])
	plain = append(plain, option)
	plain = append(plain, 0x00, 0x00, 0x00)
	plain[34] = option
	plain[35] = security
	plain[36] = 0x00
	plain[37] = command
	// mux 请求头不写地址段：与 v2fly/Xray 客户端一致，目标由命令反推。
	if command != RequestCommandMux {
		plain = append(plain, byte(dest.Port>>8), byte(dest.Port))
		plain = append(plain, encodeAddress(dest)...)
	}

	fnv1a := fnv.New32a()
	_, _ = fnv1a.Write(plain)
	sum := fnv1a.Sum32()
	plain = append(plain, byte(sum>>24), byte(sum>>16), byte(sum>>8), byte(sum))

	authID := newAuthID(cmdKey, time.Now())
	if _, err := conn.Write(sealAEADHeader(cmdKey, authID, plain)); err != nil {
		return nil, err
	}

	bodyAuth, bodyCodec, bodyPadding, err := chunkCodec(&Request{
		Security: security, Option: option, bodyKey: bodyKey, bodyIV: bodyIV,
	}, false)
	if err != nil {
		return nil, err
	}
	return &ClientSession{
		conn: conn, security: security, option: option,
		bodyIV: bodyIV, bodyKey: bodyKey, respV: respV[0],
		bodyW: newChunkWriter(conn, bodyAuth, bodyCodec, bodyPadding),
	}, nil
}

// ReadResponseHeader 读取并校验服务端响应头，准备响应体解密。
func (c *ClientSession) ReadResponseHeader() error {
	respKey := sha256Sum16(c.bodyKey[:])
	respIV := sha256Sum16(c.bodyIV[:])

	var lenBlock [18]byte
	if _, err := io.ReadFull(c.conn, lenBlock[:]); err != nil {
		return fmt.Errorf("vmess client: read resp len: %w", err)
	}
	lenPlain, err := newAESGCM(KDF16(respKey[:], KDFSaltConstAEADRespHeaderLenKey)).
		Open(nil, KDF(respIV[:], KDFSaltConstAEADRespHeaderLenIV)[:12], lenBlock[:], nil)
	if err != nil {
		return fmt.Errorf("vmess client: open resp len: %w", err)
	}
	length := int(binary.BigEndian.Uint16(lenPlain))

	payload := make([]byte, length+16)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return fmt.Errorf("vmess client: read resp payload: %w", err)
	}
	plain, err := newAESGCM(KDF16(respKey[:], KDFSaltConstAEADRespHeaderPayloadKey)).
		Open(nil, KDF(respIV[:], KDFSaltConstAEADRespHeaderPayloadIV)[:12], payload, nil)
	if err != nil {
		return fmt.Errorf("vmess client: open resp payload: %w", err)
	}
	if len(plain) < 4 || plain[0] != c.respV {
		return fmt.Errorf("vmess client: unexpected response header: got V=%d want=%d, plain=% x", plain[0], c.respV, plain)
	}

	c.respKey = respKey
	c.respIV = respIV
	c.bodyR = newChunkReader(c.conn, c.respAuth(), c.respCodec(), c.option&OptionGlobalPadding != 0)
	return nil
}

// respAuth 响应体认证器（密钥为响应派生对）。
func (c *ClientSession) respAuth() *chunkAuth {
	switch c.security {
	case SecurityChaCha20Poly1305:
		return newChunkAuth(newChaCha(GenerateChacha20Poly1305Key(c.respKey[:])), c.respIV[:])
	case SecurityNone:
		return newChunkAuth(newNoOp(), c.respIV[:])
	default:
		return newChunkAuth(newAESGCM(c.respKey[:]), c.respIV[:])
	}
}

// respCodec 响应体长度编解码（掩码用响应 IV）。
func (c *ClientSession) respCodec() sizeCodec {
	if c.option&OptionChunkMasking != 0 {
		return newShakeSizeCodec(c.respIV[:])
	}
	return plainSizeCodec{}
}

// BodyWriter 返回请求体写入器（分块加密；Close 写终结块）。
func (c *ClientSession) BodyWriter() io.WriteCloser { return c.bodyW }

// BodyReader 返回响应体读取器（分块解密）。
func (c *ClientSession) BodyReader() io.Reader { return c.bodyR }

// Close 关闭底层连接。
func (c *ClientSession) Close() error { return c.conn.Close() }

// encodeAddress 按协议写出地址段（IPv4/域名/IPv6 字面量自动识别）。
func encodeAddress(dest Target) []byte {
	if ip := parseIPBytes(dest.Host); ip != nil {
		if len(ip) == 4 {
			return append([]byte{0x01}, ip...)
		}
		return append([]byte{0x03}, ip...)
	}
	out := []byte{0x02, byte(len(dest.Host))}
	return append(out, dest.Host...)
}

// parseIPBytes 把地址文本解析为 4/16 字节 IP；非 IP 字面量返回 nil。
func parseIPBytes(host string) []byte {
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip.To16()
}

// isIPv6Literal 判断地址文本是否含冒号（IPv6 字面量的标志）。
func isIPv6Literal(host string) bool {
	for i := 0; i < len(host); i++ {
		if host[i] == ':' {
			return true
		}
	}
	return false
}

// parseIPv6 把 IPv6 文本解析为 16 字节。
func parseIPv6(host string) []byte {
	ip := net.ParseIP(host)
	if ip == nil {
		panic("vmess: bad ipv6 literal " + host)
	}
	return ip.To16()
}

// parseIPv4 把 IPv4 文本解析为 4 字节。
func parseIPv4(host string) []byte {
	ip := net.ParseIP(host)
	if ip == nil {
		panic("vmess: bad ipv4 literal " + host)
	}
	return ip.To4()
}
