package protocol

import (
	"fmt"
	"io"
)

// WriteReply 尽力回一个结果帧；写失败只说明对端已走，不再上报错误。
func WriteReply(w io.Writer, typ Type, res Reply) {
	frame, err := NewReplyFrame(typ, res)
	if err != nil {
		return
	}
	_ = NewEncoder(w).WriteFrame(frame)
}

// ReadPayload 读完帧载荷并校验 CRC。
func ReadPayload(f *Frame) ([]byte, error) {
	payload, err := io.ReadAll(f.Body)
	if cerr := f.CloseBody(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("protocol: read frame payload: %w", err)
	}
	return payload, nil
}
