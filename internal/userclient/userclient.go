package userclient

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/tcp"
)

// DialTimeout 是连接建立（TCP + 认证握手）的超时；服务端各阶段认证超时为 5s。
const DialTimeout = 5 * time.Second

// AuthError 表示 UserAuth 被拒绝：Reply 是服务端应答（含原因与回带的客户端配置），
// 此时会话已关闭。
type AuthError struct {
	Reply protocol.Reply
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("user auth rejected: %s", e.Reply.Reason)
}

// Client 是一条 UserAuth 已认证的用户多路复用会话。
type Client struct {
	conn    net.Conn
	session tcp.Session
}

// Dial 拨入数据面并以 user_key 完成 UserAuth 认证。req 可携带用户自报信息；
// cfg 为多路复用会话配置，必须与服务端一致（keepalive 尤其要两端对称），nil 用
// 默认值；muxer 选择多路复用后端，nil 用 tcp.DefaultMuxer，必须与服务端一致。
// 认证被拒绝时返回 *AuthError。
func Dial(addr string, req protocol.UserAuthRequest, cfg *tcp.SessionConfig, muxer tcp.Muxer) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("userclient: dial %s: %w", addr, err)
	}
	if muxer == nil {
		muxer = tcp.DefaultMuxer()
	}
	if cfg == nil {
		cfg = tcp.DefaultSessionConfig()
	}
	session, err := muxer.Client(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("userclient: %s client: %w", muxer.Name(), err)
	}
	c := &Client{conn: conn, session: session}

	// 认证握手独占一条流，限时 5s，完成后关流（认证是一次性的）。
	auth, err := session.OpenStream()
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("userclient: open auth stream: %w", err)
	}
	if err := auth.SetDeadline(time.Now().Add(DialTimeout)); err != nil {
		c.Close()
		return nil, fmt.Errorf("userclient: set auth deadline: %w", err)
	}
	uaf, err := protocol.NewUserAuthFrame(req)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("userclient: build user auth frame: %w", err)
	}
	if err := protocol.NewEncoder(auth).WriteFrame(uaf); err != nil {
		c.Close()
		return nil, fmt.Errorf("userclient: write user auth: %w", err)
	}
	af, err := protocol.NewDecoder(auth).ReadFrame()
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("userclient: read auth result: %w", err)
	}
	afb, err := protocol.ReadPayload(af)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("userclient: read auth payload: %w", err)
	}
	reply, err := protocol.ParseReply(afb)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("userclient: parse auth reply: %w", err)
	}
	if !reply.OK {
		c.Close()
		return nil, &AuthError{Reply: reply}
	}
	return c, nil
}

// Forward 开一条 Forward 隧道到 dest；成功后该流承载裸字节（先写请求、再读响应）。
// 被拒绝（ACL、客户端离线等）时关闭流并返回带原因的 error。
func (c *Client) Forward(dest protocol.Address) (net.Conn, error) {
	fwd, err := c.session.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("userclient: open forward stream: %w", err)
	}
	ff, err := protocol.NewForwardFrame(dest)
	if err != nil {
		_ = fwd.Close()
		return nil, fmt.Errorf("userclient: build forward frame: %w", err)
	}
	if err := protocol.NewEncoder(fwd).WriteFrame(ff); err != nil {
		_ = fwd.Close()
		return nil, fmt.Errorf("userclient: write forward frame: %w", err)
	}
	f, err := protocol.NewDecoder(fwd).ReadFrame()
	if err != nil {
		_ = fwd.Close()
		return nil, fmt.Errorf("userclient: read forward result: %w", err)
	}
	fb, err := protocol.ReadPayload(f)
	if err != nil {
		_ = fwd.Close()
		return nil, fmt.Errorf("userclient: read forward payload: %w", err)
	}
	reply, err := protocol.ParseReply(fb)
	if err != nil {
		_ = fwd.Close()
		return nil, fmt.Errorf("userclient: parse forward reply: %w", err)
	}
	if err := f.CloseBody(); err != nil {
		_ = fwd.Close()
		return nil, fmt.Errorf("userclient: forward reply crc: %w", err)
	}
	if !reply.OK {
		_ = fwd.Close()
		return nil, errors.New("userclient: forward denied: " + reply.Reason)
	}
	return fwd, nil
}

// Close 关闭用户会话与底层连接。
func (c *Client) Close() error {
	if c.session != nil {
		_ = c.session.Close()
	}
	return c.conn.Close()
}
