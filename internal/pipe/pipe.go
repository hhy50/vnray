// Package pipe 提供支持优雅关闭和立即中断的有界内存管道。
package pipe

import (
	"io"
	"sync"

	"github.com/smallnest/ringbuffer"
)

// pipeLimit 是默认缓冲上限。写满后写端阻塞，形成背压。
const pipeLimit = 64 * 1024

type pipe struct {
	mu      sync.Mutex
	writeMu sync.Mutex
	cond    *sync.Cond
	buf     *ringbuffer.RingBuffer
	err     error
}

// New 创建一条管道，返回 (读端, 写端)。
func New() (*Reader, *Writer) {
	p := &pipe{}
	p.cond = sync.NewCond(&p.mu)
	return &Reader{p}, &Writer{p}
}

// Reader 是管道读端，实现 io.Reader。
type Reader struct{ p *pipe }

// Writer 是管道写端，实现 io.Writer。
type Writer struct{ p *pipe }

// Read 取数据填入 p；没有数据时阻塞，直到有数据或管道关闭。
func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	q := r.p
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if q.buf != nil && !q.buf.IsEmpty() {
			n, err := q.buf.Read(p)
			q.cond.Broadcast()
			return n, err
		}
		if q.err != nil {
			return 0, q.err
		}
		q.cond.Wait()
	}
}

// Write 写入一块数据；缓冲满时阻塞（背压）。
func (w *Writer) Write(b []byte) (int, error) {
	q := w.p
	// 大写入会分段等待；保持整次 Write 串行，避免并发写入交错。
	q.writeMu.Lock()
	defer q.writeMu.Unlock()
	q.mu.Lock()
	defer q.mu.Unlock()
	written := 0
	for {
		if q.err != nil {
			return written, io.ErrClosedPipe
		}
		if written == len(b) {
			return written, nil
		}
		if q.buf == nil {
			q.buf = ringbuffer.New(pipeLimit)
		}
		n, err := q.buf.Write(b[written:])
		written += n
		if n > 0 {
			q.cond.Broadcast()
		}
		switch err {
		case nil:
			return written, nil
		case ringbuffer.ErrIsFull, ringbuffer.ErrTooMuchDataToWrite:
			q.cond.Wait()
		default:
			return written, err
		}
	}
}

// Close 优雅关闭写端，等价于 CloseWithError(nil)。
func (w *Writer) Close() error {
	return w.CloseWithError(nil)
}

// CloseWithError 优雅关闭写端并携带原因；缓冲数据仍可读，读空后收到 err。
func (w *Writer) CloseWithError(err error) error {
	if err == nil {
		err = io.EOF
	}
	w.p.close(err, false)
	return nil
}

// Interrupt 立即中断（写端触发）。
func (w *Writer) Interrupt() {
	w.p.close(io.ErrClosedPipe, true)
}

// Interrupt 立即中断（读端也可触发）。
func (r *Reader) Interrupt() {
	r.p.close(io.ErrClosedPipe, true)
}

// close 保留首次关闭的终态，后续清理不能覆盖待传递的错误或尾数据。
func (p *pipe) close(err error, discard bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return
	}
	p.err = err
	if discard {
		p.buf = nil
	}
	p.cond.Broadcast()
}
