package pipe

import (
	"io"
	"sync"
)

type state byte

const (
	open state = iota
	// closed：Close 过，剩余数据可读，然后收 EOF/err。
	closed
	// errord：Interrupt 过，丢弃一切。
	errord
)

// pipeLimit 是默认缓冲上限。写满后写端阻塞，形成背压。
const pipeLimit = 64 * 1024

type pipe struct {
	mu   sync.Mutex
	cond *sync.Cond
	// data 是待消费的数据块队列。
	data  [][]byte
	size  int
	state state
	// err 为 CloseWithError 携带的原因，仅在 closed 状态下有意义。
	err error
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
	q := r.p
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if len(q.data) > 0 {
			n := copy(p, q.data[0])
			if n == len(q.data[0]) {
				q.data[0] = nil
				q.data = q.data[1:]
			} else {
				q.data[0] = q.data[0][n:]
			}
			q.size -= n
			q.cond.Broadcast()
			return n, nil
		}
		switch q.state {
		case closed:
			if q.err != nil {
				return 0, q.err
			}
			return 0, io.EOF
		case errord:
			return 0, io.ErrClosedPipe
		}
		q.cond.Wait()
	}
}

// Write 写入一块数据；缓冲满时阻塞（背压）。
func (w *Writer) Write(b []byte) (int, error) {
	q := w.p
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if q.state != open {
			return 0, io.ErrClosedPipe
		}
		if q.size < pipeLimit {
			chunk := make([]byte, len(b))
			copy(chunk, b)
			q.data = append(q.data, chunk)
			q.size += len(chunk)
			q.cond.Broadcast()
			return len(chunk), nil
		}
		q.cond.Wait()
	}
}

// Close 优雅关闭写端，等价于 CloseWithError(nil)。
func (w *Writer) Close() error {
	return w.closeWithError(nil)
}

// CloseWithError 优雅关闭写端并携带原因；缓冲数据仍可读，读空后收到 err。
func (w *Writer) CloseWithError(err error) error {
	if err == nil {
		err = io.EOF
	}
	return w.closeWithError(err)
}

func (w *Writer) closeWithError(err error) error {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	if w.p.state != open {
		return nil
	}
	w.p.state = closed
	w.p.err = err
	w.p.cond.Broadcast()
	return nil
}

// Interrupt 立即中断（写端触发）。
func (w *Writer) Interrupt() {
	_ = w.p.shutdown(true)
}

// Interrupt 立即中断（读端也可触发）。
func (r *Reader) Interrupt() {
	_ = r.p.shutdown(true)
}

func (p *pipe) shutdown(interrupt bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != open {
		return nil
	}
	if interrupt {
		p.state = errord
		p.data = nil
		p.size = 0
	} else {
		p.state = closed
	}
	p.cond.Broadcast()
	return nil
}
