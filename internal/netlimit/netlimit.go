// Package netlimit 给监听器加并发连接上限：协程数因此有硬上限，而不是随客户端
// 数量漂移。
package netlimit

import (
	"net"
	"sync"
)

// DefaultMaxConns 是单个 HTTP 服务同时持有的连接数上限。管理台与诊断端点的并发
// 都很低，这个值只用于兜底异常客户端与外部刷连接。
const DefaultMaxConns = 32

// Listener 在连接数占满后阻塞 Accept，新连接留在内核 backlog，由内核在 backlog
// 满时拒绝。协程数因此稳定在「上限 + 常驻后台」。
type Listener struct {
	net.Listener
	sem chan struct{}
}

// New 包装 ln 限制同时打开的连接数；max <= 0 时原样返回。
func New(ln net.Listener, max int) net.Listener {
	if max <= 0 {
		return ln
	}
	return &Listener{Listener: ln, sem: make(chan struct{}, max)}
}

// Accept 接受连接并占用一个名额；连接关闭时归还。
func (l *Listener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.sem <- struct{}{}
	return &limitedConn{Conn: conn, release: func() { <-l.sem }}, nil
}

// limitedConn 保证名额只归还一次：Close 可能被 http.Server 与调用方各调一遍。
type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

// Close 关闭连接并归还名额。
func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
