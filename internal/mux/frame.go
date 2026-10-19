package mux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

// 子连接状态与选项位，取值与 v2ray 协议规范一致。
const (
	StatusNew       byte = 0x01
	StatusKeep      byte = 0x02
	StatusEnd       byte = 0x03
	StatusKeepAlive byte = 0x04
)

// 帧选项位：Data 表示帧带数据，Error 仅用于 End 帧（异常收尾）。
const (
	OptionData  byte = 0x01
	OptionError byte = 0x02
)

// New 帧里的网络类型；本项目只服务 TCP，UDP 子连接一律拒绝。
const (
	NetworkTCP byte = 0x01
	NetworkUDP byte = 0x02
)

// 地址类型字节，与 VMess 地址块一致。
const (
	addrTypeIPv4   byte = 0x01
	addrTypeDomain byte = 0x02
	addrTypeIPv6   byte = 0x03
)

// maxMetaLen 是元数据段长度上限（v2ray 同值 512）：域名最长 255B，512 足够容纳
// 全部合法帧，同时把畸形长度膨胀挡在读取之前。
const maxMetaLen = 512

// maxFrameData 是单帧数据上限：v2ray 客户端按 8KiB 切片，取同值。
const maxFrameData = 8 * 1024

// Target 是子连接的目标地址。
type Target struct {
	Host string
	Port int
}

// String 实现 fmt.Stringer。
func (t Target) String() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// Frame 是解码后的一个协议帧。
type Frame struct {
	SessionID uint16
	Status    byte
	Option    byte
	// Network 仅 StatusNew 有效。
	Network byte
	// Target 仅 StatusNew 有效。
	Target Target
	// Data 在 OptionData 置位时非空；长度为零的合法数据帧也存在（对端探测用）。
	Data []byte
}

// HasData 判断帧是否携带数据段。
func (f *Frame) HasData() bool { return f.Option&OptionData != 0 }

// readFrame 从 r 读出一个完整帧。数据段在元数据之后：2B 长度 + 数据本体，
// 与 v2ray 的 PlainChunkSizeParser 布局一致。
func readFrame(r io.Reader) (*Frame, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	metaLen := binary.BigEndian.Uint16(lenBuf[:])
	if metaLen > maxMetaLen {
		return nil, fmt.Errorf("mux: invalid frame metadata length %d", metaLen)
	}
	meta := make([]byte, metaLen)
	if _, err := io.ReadFull(r, meta); err != nil {
		return nil, err
	}

	f := &Frame{}
	if len(meta) < 4 {
		return nil, errors.New("mux: frame metadata too short")
	}
	f.SessionID = binary.BigEndian.Uint16(meta[0:2])
	f.Status = meta[2]
	f.Option = meta[3]

	if f.Status == StatusNew {
		if len(meta) < 8 {
			return nil, errors.New("mux: new frame metadata too short")
		}
		f.Network = meta[4]
		f.Target.Port = int(binary.BigEndian.Uint16(meta[5:7]))
		host, n, err := readAddress(meta[7:])
		if err != nil {
			return nil, err
		}
		if 7+n != len(meta) {
			return nil, errors.New("mux: trailing bytes after address in new frame")
		}
		f.Target.Host = host
	}

	if f.HasData() {
		var dataLen [2]byte
		if _, err := io.ReadFull(r, dataLen[:]); err != nil {
			return nil, err
		}
		f.Data = make([]byte, binary.BigEndian.Uint16(dataLen[:]))
		if _, err := io.ReadFull(r, f.Data); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// readAddress 解析地址段：类型 1B + 地址本体，返回主机文本与消耗的字节数。
func readAddress(b []byte) (string, int, error) {
	if len(b) < 1 {
		return "", 0, errors.New("mux: truncated address")
	}
	switch b[0] {
	case addrTypeIPv4:
		if len(b) < 5 {
			return "", 0, errors.New("mux: truncated ipv4 address")
		}
		return net.IP(b[1:5]).String(), 5, nil
	case addrTypeDomain:
		if len(b) < 2 {
			return "", 0, errors.New("mux: truncated domain address")
		}
		n := int(b[1])
		if len(b) < 2+n {
			return "", 0, errors.New("mux: truncated domain address")
		}
		return string(b[2 : 2+n]), 2 + n, nil
	case addrTypeIPv6:
		if len(b) < 17 {
			return "", 0, errors.New("mux: truncated ipv6 address")
		}
		return net.IP(b[1:17]).String(), 17, nil
	default:
		return "", 0, fmt.Errorf("mux: unknown address type %d", b[0])
	}
}

// writeFrame 一次写出一个完整帧（元数据 + 可选数据段）。调用方持写锁时调用，
// 保证多子连接并发写出时帧不会交错。
func writeFrame(w io.Writer, id uint16, status, option byte, data []byte) error {
	meta := make([]byte, 4)
	binary.BigEndian.PutUint16(meta[0:2], id)
	meta[2] = status
	meta[3] = option

	buf := make([]byte, 2, 2+len(meta)+2+len(data))
	binary.BigEndian.PutUint16(buf[0:2], uint16(len(meta)))
	buf = append(buf, meta...)
	if option&OptionData != 0 {
		var dataLen [2]byte
		binary.BigEndian.PutUint16(dataLen[:], uint16(len(data)))
		buf = append(buf, dataLen[:]...)
		buf = append(buf, data...)
	}
	_, err := w.Write(buf)
	return err
}
