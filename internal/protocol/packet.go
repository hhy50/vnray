package protocol

// 隧道级数据报封帧：在一条流式连接上承载 [2B 大端长度][datagram]*，与 VLESS UDP 及
// v2ray 的长度前缀同构。服务端插件用它在 DMP 隧道与 vless 入站之间透传 datagram，
// vmess 与 mux 入站则各自原生实现 DatagramConn（chunk / 帧本身就是数据报边界）。

import (
	"errors"
	"io"
)

// MaxDatagram 是单个数据报的上限（长度字段为 2B，IPv4 UDP 上限 65507 天然满足）。
const MaxDatagram = 65535

// DatagramConn 是一个双向数据报端点：ReadPacket 读一个完整 datagram，WritePacket
// 写一个完整 datagram。帧与帧之间不共享边界，每次调用对应恰好一个数据报。
type DatagramConn interface {
	ReadPacket() ([]byte, error)
	WritePacket([]byte) error
	Close() error
}

// datagramStream 把 io.ReadWriteCloser 解释成 [2B 长度][datagram]* 流。
type datagramStream struct {
	rw io.ReadWriteCloser
}

// NewDatagramConn 返回在 rw 上做 [2B 大端长度] 封帧的数据报端点。vless 入站侧与
// DMP 隧道侧均为同一帧格式，可直接透传；vmess/mux 入站在各自包内原生实现 DatagramConn。
func NewDatagramConn(rw io.ReadWriteCloser) DatagramConn {
	return &datagramStream{rw: rw}
}

// ReadPacket 读一个大端长度前缀后接的完整数据报；流干净结束时返回 io.EOF。
func (d *datagramStream) ReadPacket() ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(d.rw, lenBuf[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, protocolError("read datagram length: %v", err)
	}
	n := int(getUint16(lenBuf[:]))
	if n > MaxDatagram {
		return nil, protocolError("datagram too long: %d bytes (max %d)", n, MaxDatagram)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(d.rw, payload); err != nil {
		return nil, protocolError("read datagram: %v", err)
	}
	return payload, nil
}

// WritePacket 写一个数据报（以前缀 2B 长度 + 负载）。超长报直接拒绝而非静默截断。
func (d *datagramStream) WritePacket(payload []byte) error {
	if len(payload) > MaxDatagram {
		return protocolError("datagram too long: %d bytes (max %d)", len(payload), MaxDatagram)
	}
	var lenBuf [2]byte
	putUint16(lenBuf[:], uint16(len(payload)))
	if _, err := d.rw.Write(lenBuf[:]); err != nil {
		return err
	}
	return writeAll(d.rw, payload)
}

// Close 关闭底层连接。
func (d *datagramStream) Close() error { return d.rw.Close() }
