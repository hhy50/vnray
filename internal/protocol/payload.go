package protocol

import (
	"hash"
	"io"
)

// Payload 是帧载荷的流式读取端，并在载荷读完后负责读取与校验 CRC 尾。
type Payload struct {
	// src 为 TeeReader(LimitReader(underlying), hasher)：读到即计入 CRC。
	src        io.Reader
	underlying io.Reader
	hasher     hash.Hash32
	remaining  uint32
	closed     bool
}

// CloseBody 关闭帧体并校验 CRC。
//
// 仅对 Decoder.ReadFrame 解码出的帧（Body 为 *Payload）有效；Encode 侧手工
// 构造的帧 Body 不是 *Payload，调用为空操作。这样调用方无需做类型断言。
func (f *Frame) CloseBody() error {
	if p, ok := f.Body.(*Payload); ok {
		return p.Close()
	}
	return nil
}

// newPayload 构造长度为 length 的载荷读取端；读取的字节同步喂给 hasher。
func newPayload(underlying io.Reader, length uint32, hasher hash.Hash32) *Payload {
	limited := io.LimitReader(underlying, int64(length))
	teed := io.TeeReader(limited, hasher)
	return &Payload{src: teed, underlying: underlying, hasher: hasher, remaining: length}
}

// Read 读取载荷字节；读完 PayloadLen 后返回 io.EOF（此时 CRC 尾尚未读取）。
func (p *Payload) Read(b []byte) (int, error) {
	if p.remaining == 0 {
		return 0, io.EOF
	}
	n, err := p.src.Read(b)
	p.remaining -= uint32(n)
	if err == io.EOF && p.remaining != 0 {
		// 对端给的载荷比头部声明的短。
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// Close 消费掉剩余载荷，读取尾部并校验 CRC。
// 幂等：首次校验通过后置位，后续调用为空操作。
func (p *Payload) Close() error {
	if p.closed {
		return nil
	}
	if _, err := io.Copy(io.Discard, p); err != nil {
		return err
	}
	var crcBuf [crcLen]byte
	if _, err := io.ReadFull(p.underlying, crcBuf[:]); err != nil {
		return protocolError("read crc trailer: %v", err)
	}
	if got, want := getUint32(crcBuf[:]), p.hasher.Sum32(); got != want {
		return protocolError("crc mismatch: got %08x want %08x", got, want)
	}
	p.closed = true
	return nil
}
