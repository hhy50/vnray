package mux

import (
	"io"
	"sync"
)

// Writer 把子连接的响应写回主连接。多条子连接的响应协程并发调用，帧的构建与
// 写出在锁内一次完成，帧与帧不会交错；首次写错误后短路，后续写出直接复用该错误。
type Writer struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

// NewWriter 创建响应写出器。
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// writeData 以 Keep 数据帧写出一块数据，超过单帧上限自动分帧。
func (w *Writer) writeData(id uint16, p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxFrameData)
		if err := w.writeLocked(id, StatusKeep, OptionData, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// writePacket 以单个 Keep 数据帧写出一个完整数据报（UDP 子连接用，保持数据报唯一，
// 不做 8K 分包）。数据报长度须能放进帧的 2B 数据长度字段。
func (w *Writer) writePacket(id uint16, datagram []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeLocked(id, StatusKeep, OptionData, datagram)
}

// writeEnd 发送 End 帧；errFlag 置位时带 OptionError（对端据此异常收尾）。
func (w *Writer) writeEnd(id uint16, errFlag bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeLocked(id, StatusEnd, optionOf(errFlag), nil)
}

// writeKeepAlive 发送 KeepAlive 帧（主连接空闲保活；数据段一律不带）。
func (w *Writer) writeKeepAlive(id uint16) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeLocked(id, StatusKeepAlive, 0, nil)
}

// writeLocked 调用方须持锁。
func (w *Writer) writeLocked(id uint16, status, option byte, data []byte) error {
	if w.err != nil {
		return w.err
	}
	if err := writeFrame(w.w, id, status, option, data); err != nil {
		w.err = err
		return err
	}
	return nil
}

// optionOf 把错误标记翻成选项位。
func optionOf(errFlag bool) byte {
	if errFlag {
		return OptionError
	}
	return 0
}
