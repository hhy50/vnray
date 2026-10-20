package protocol

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
)

// Magic 是帧头魔数 0x2026；纯标识、不承载版本语义，对端不匹配即按协议错拒绝（见 ADR 0014）。
var Magic = [2]byte{0x20, 0x26}

// Type 是消息类型；占 2 字节，编号只增不改。
type Type uint16

// 消息类型枚举。
//
// 编号 1/2/3 曾是请求-响应模型（Data/DataResult/Error），已整体退役且**永不复用**，
// 详见 ADR 0018；新类型从 13 起继续递增。
const (
	// Ping 是 server→client 的心跳请求；载荷为 8 字节大端 unix 毫秒发送时刻。
	Ping Type = 4
	// Pong 是 client→server 的心跳应答，载荷原样回显 Ping 的时间戳。
	Pong Type = 5
	// Auth 是 client→server 的认证帧，载荷为密钥与客户端自报信息的 JSON。
	Auth Type = 6
	// AuthResult 是 server→client 的认证应答，载荷为通过与否及拒绝原因的 JSON；
	// 同时用作 UserAuth 的应答，用户路径还回带绑定的客户端配置。
	AuthResult Type = 7
	// Forward 是用户→server 的转发请求，载荷以地址块开头；
	// 用户身份在会话级 UserAuth 已确认，转发按其绑定的客户端选路。
	Forward Type = 8
	// ForwardResult 是 server→用户的转发应答。
	ForwardResult Type = 9
	// Tunnel 是 server→client 的隧道请求，载荷以地址块开头；之后该流承载裸字节。
	Tunnel Type = 10
	// TunnelResult 是 client→server 的隧道应答（拨号成功与否）。
	TunnelResult Type = 11
	// UserAuth 是 user→server 的用户认证帧，载荷为用户密钥与用户端自报信息的 JSON；
	// 通过后该会话成为用户会话，后续 Forward 流按其绑定的客户端选路。
	UserAuth Type = 12
	// PortMap 是 server→client 的端口映射请求，载荷为地址块（客户端侧的本地拨号
	// 目标）；客户端本地直连、不经其 dispatcher，应答复用 TunnelResult，之后该流
	// 承载裸字节。数据面与 Tunnel 完全同构，仅拨号语义不同（见 ADR 0030）。
	PortMap Type = 13
	// ConfigPush 是 server→client 的全量配置下发：载荷为 PushConfig JSON 快照，
	// 客户端收到后整体应用。当前只有自定义 host，后续新增配置项直接在该快照里扩展
	// 字段（见 ADR 0033）。管理台配置后经此推送，客户端重连时也随 AuthResult 一并带过。
	ConfigPush Type = 14
)

// header 是固定头的内存视图（10 字节：魔数2+类型2+标志1+保留3+载荷长2）。
const (
	// headerLen 是固定头长度：magic2+type2+flags1+hostlen1+port2+payloadlen2。
	headerLen = 10
	// crcLen 是尾部校验长度。
	crcLen = 4
	// heartbeatPayloadLen 是心跳帧载荷长度：8 字节大端 unix 毫秒。
	heartbeatPayloadLen = 8
)

// ErrProtocol 是协议违规的哨兵错误，调用方可用 errors.Is 判断。
var ErrProtocol = errors.New("protocol error")

// protocolError 构造一个包裹 ErrProtocol 的协议错误。
func protocolError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, args...))
}

// validType 判断类型是否为已定义的枚举。
func validType(t Type) bool {
	switch t {
	case Ping, Pong, Auth, AuthResult, Forward, ForwardResult, Tunnel, TunnelResult, UserAuth, PortMap, ConfigPush:
		return true
	default:
		return false
	}
}

// header 是固定头的内存视图（10 字节：魔数2+类型2+标志1+保留3+载荷长2）。
// b[4] 是标志位，语义见 Flag* 常量；保留 3 字节（b[5:8]）原本是主机长度与端口——
// 目标挪进载荷后留作将来扩展：编码时置零、解码时忽略（不校验，避免给将来复用埋雷）。
type header struct {
	Type       Type
	Flags      byte
	PayloadLen uint32 // 线路上是 2 字节字段
}

// 标志位：语义按位定义，只增不改。目前仅 UDP 隧道使用；其余帧恒为 0。
const FlagUDP byte = 0x01

// encode 把固定头写入 b（len(b) 必须 == headerLen）。
func (h header) encode(b []byte) {
	b[0], b[1] = Magic[0], Magic[1]
	putUint16(b[2:4], uint16(h.Type))
	b[4] = h.Flags
	b[5], b[6], b[7] = 0, 0, 0
	putUint16(b[8:10], uint16(h.PayloadLen))
}

// decodeHeader 从 10 字节固定头解析出头部视图；魔数与类型在此校验。
func decodeHeader(b []byte) (header, error) {
	if b[0] != Magic[0] || b[1] != Magic[1] {
		return header{}, protocolError("bad magic %02x%02x, want %02x%02x", b[0], b[1], Magic[0], Magic[1])
	}
	t := Type(getUint16(b[2:4]))
	if !validType(t) {
		return header{}, protocolError("unknown message type %d", t)
	}
	return header{Type: t, Flags: b[4], PayloadLen: uint32(getUint16(b[8:10]))}, nil
}

// Frame 是一帧的逻辑视图。目标不在头里：带目标消息（Forward/Tunnel）的
// 载荷以 Address 块开头（见 ReadAddress）。
type Frame struct {
	// Type 是消息类型。
	Type Type
	// Flags 是从固定头带来的标志位（见 Flag*）。仅 Tunnel 帧目前会带 FlagUDP。
	Flags byte
	// PayloadLen 是载荷字节数；线路上是 2 字节字段，因此单帧上限 65535。
	PayloadLen uint32
	// Body 是载荷的读写端：写入时为待读载荷，读取时为 *Payload 流式载荷。
	Body io.Reader
}

// Address 是带目标消息载荷开头的地址块：主机长(1)+主机(N)+端口(2)。
// 主机为域名或 IP 字面量（IPv6 用规范文本，不带方括号）。
type Address struct {
	Host string
	Port uint16
}

// Bytes 编码地址块；主机超过 255 字节时报错（不静默截断）。
func (a Address) Bytes() ([]byte, error) {
	if len(a.Host) > math.MaxUint8 {
		return nil, protocolError("host too long: %d bytes (max %d)", len(a.Host), math.MaxUint8)
	}
	b := make([]byte, 1+len(a.Host)+2)
	b[0] = byte(len(a.Host))
	copy(b[1:], a.Host)
	putUint16(b[1+len(a.Host):], a.Port)
	return b, nil
}

// ReadAddress 从载荷流读出地址块；载荷的剩余部分（若有）继续从 r 读。
func ReadAddress(r io.Reader) (Address, error) {
	var l [1]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return Address{}, protocolError("read address host length: %v", err)
	}
	host := make([]byte, l[0])
	if _, err := io.ReadFull(r, host); err != nil {
		return Address{}, protocolError("read address host: %v", err)
	}
	var p [2]byte
	if _, err := io.ReadFull(r, p[:]); err != nil {
		return Address{}, protocolError("read address port: %v", err)
	}
	return Address{Host: string(host), Port: getUint16(p[:])}, nil
}

// NewBufferedFrame 用一段已在内存的字节构造帧；Body 自动指向该字节切片。
// payload 可为 nil（PayloadLen=0）。
func NewBufferedFrame(t Type, payload []byte) *Frame {
	var body io.Reader
	if len(payload) > 0 {
		body = bytes.NewReader(payload)
	}
	return &Frame{
		Type:       t,
		PayloadLen: uint32(len(payload)),
		Body:       body,
	}
}

// NewHeartbeatFrame 构造心跳帧，载荷为 8 字节大端 unix 毫秒时间戳。
//
// 时间戳放载荷而不是固定头：固定头对所有消息等长，而只有心跳需要时间戳。服务端发 Ping
// 时填本端当前时刻，客户端回 Pong 时原样回显（读端见 ReadHeartbeatTimestamp）。
func NewHeartbeatFrame(t Type, timestampMillis int64) *Frame {
	var ts [heartbeatPayloadLen]byte
	putUint64(ts[:], uint64(timestampMillis))
	return NewBufferedFrame(t, ts[:])
}

// ReadHeartbeatTimestamp 读出血心跳帧载荷里的毫秒时间戳；读完顺带校验 CRC 尾。
func ReadHeartbeatTimestamp(f *Frame) (int64, error) {
	if f.PayloadLen != heartbeatPayloadLen {
		return 0, protocolError("heartbeat payload is %d bytes, want %d", f.PayloadLen, heartbeatPayloadLen)
	}
	payload, err := ReadPayload(f)
	if err != nil {
		return 0, err
	}
	return int64(getUint64(payload)), nil
}

// Encoder 把帧编码写入底层 io.Writer.
type Encoder struct{ w io.Writer }

// NewEncoder 创建绑定 w 的编码器。
func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

// WriteFrame 写入一帧。f.Body 必须恰好提供 PayloadLen 字节。
// 载荷在写出的同时喂给 CRC，无需在内存缓冲整段载荷。
func (e *Encoder) WriteFrame(f *Frame) error {
	if !validType(f.Type) {
		return protocolError("unknown message type %d", f.Type)
	}
	if f.PayloadLen > math.MaxUint16 {
		return protocolError("payload too long: %d bytes (max %d)", f.PayloadLen, math.MaxUint16)
	}
	if f.Body == nil && f.PayloadLen != 0 {
		return protocolError("nil body but payload length is %d", f.PayloadLen)
	}

	// 带目标消息的载荷必须以 Address 块开头（构造方负责），主机长度上限在
	// Address.Bytes 里把关；这里只校验载荷总宽。
	h := header{Type: f.Type, Flags: f.Flags, PayloadLen: f.PayloadLen}
	head := make([]byte, headerLen)
	h.encode(head)

	hasher := crc32.NewIEEE()
	if err := writeAll(io.MultiWriter(e.w, hasher), head); err != nil {
		return fmt.Errorf("write frame header: %w", err)
	}

	// 严格复制 PayloadLen 字节：少给会以 EOF/ErrUnexpectedEOF 暴露。
	n, err := io.CopyN(io.MultiWriter(e.w, hasher), f.Body, int64(f.PayloadLen))
	if err != nil {
		return fmt.Errorf("write frame payload: %w", err)
	}
	if uint32(n) != f.PayloadLen {
		return protocolError("payload short: header says %d but body gave %d", f.PayloadLen, n)
	}

	var crcBuf [crcLen]byte
	putUint32(crcBuf[:], hasher.Sum32())
	if err := writeAll(e.w, crcBuf[:]); err != nil {
		return fmt.Errorf("write frame crc: %w", err)
	}
	return nil
}

// Decoder 从底层 io.Reader 读取并解码帧。
type Decoder struct{ r io.Reader }

// NewDecoder 创建绑定 r 的解码器。
func NewDecoder(r io.Reader) *Decoder { return &Decoder{r: r} }

// ReadFrame 读取一帧。
//
// 返回时载荷尚未消费：调用方从 f.Body 读取恰好 PayloadLen 字节（带目标消息先过
// ReadAddress），然后必须调用 f.Body.Close()，以读取并校验尾部 CRC。
// 流在帧之间干净结束时返回 io.EOF。
func (d *Decoder) ReadFrame() (*Frame, error) {
	var headBuf [headerLen]byte
	if _, err := io.ReadFull(d.r, headBuf[:]); err != nil {
		// 零字节是正常结束；读到一半则是协议层面的截断。
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, protocolError("read header: %v", err)
	}
	h, err := decodeHeader(headBuf[:])
	if err != nil {
		return nil, err
	}

	hasher := crc32.NewIEEE()
	_, _ = hasher.Write(headBuf[:])

	frame := &Frame{
		Type:       h.Type,
		Flags:      h.Flags,
		PayloadLen: h.PayloadLen,
	}
	frame.Body = newPayload(d.r, frame.PayloadLen, hasher)
	return frame, nil
}

// writeAll 完整写入 b，避免底层 Writer 部分写。
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}
