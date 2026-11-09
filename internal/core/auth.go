package core

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/secure"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/transport"
)

// AuthTimeout 是握手各阶段的等待上限（服务端等认证流、两端等对端帧）。
const AuthTimeout = 5 * time.Second

// ErrAuthRejected 表示服务端拒绝了这次认证。
var ErrAuthRejected = errors.New("authentication rejected")

// errKeyMismatch 是密钥不匹配的原因，会作为拒绝原因回给客户端。
var errKeyMismatch = errors.New("key mismatch")

// Handshake 在安全传输握手后接收首次配置上报。
func (s *ConnectorService) Handshake(session tcp.Session) (tcp.SessionInfo, error) {
	ctx := s.context()
	if ctx == nil {
		return tcp.SessionInfo{}, ErrAuthRejected
	}
	stream, frame, err := initialFrame(session, protocol.ConfigReport)
	if err != nil {
		return tcp.SessionInfo{}, err
	}
	defer func() { _ = stream.Close() }()
	info, err := s.readClientReport(ctx, session, frame, "", false)
	if err != nil {
		protocol.WriteReply(stream, protocol.ConfigReportResult, protocol.Reply{Reason: err.Error()})
		return tcp.SessionInfo{}, err
	}
	client := transport.NewSession(session, session.RemoteAddr().String(), info)
	s.saveClientReport(ctx, client, info)
	// 日志级别、并发上限与下发快照一次读出：握手路径上少两次点查。
	var runtime transport.ClientRuntime
	if s.sessionCfg != nil {
		runtime = s.sessionCfg.ClientRuntime(ctx, info.Name)
		client.SetClientConcurrency(runtime.Concurrency)
	}
	if err := client.SendConfigReportResult(stream, func() protocol.PushConfig { return runtime.Push }); err != nil {
		return tcp.SessionInfo{}, err
	}
	return tcp.SessionInfo{
		Session: session,
		Remote:  session.RemoteAddr().String(),
		Kind:    tcp.KindClient,
		Name:    info.Name,
		Log:     logging.NewScoped(runtime.LogLevel, "client", info.Name),
		Meta:    client,
	}, nil
}

func initialFrame(session tcp.Session, want protocol.Type) (tcp.Stream, *protocol.Frame, error) {
	if err := session.SetDeadline(time.Now().Add(AuthTimeout)); err != nil {
		return nil, nil, err
	}
	defer func() { _ = session.SetDeadline(time.Time{}) }()
	stream, err := session.AcceptStream()
	if err != nil {
		return nil, nil, err
	}
	if err := stream.SetDeadline(time.Now().Add(AuthTimeout)); err != nil {
		_ = stream.Close()
		return nil, nil, err
	}
	frame, err := protocol.NewDecoder(stream).ReadFrame()
	if err == nil && frame.Type != want {
		_ = frame.CloseBody()
		err = fmt.Errorf("%w: expected frame %d, got %d", protocol.ErrProtocol, want, frame.Type)
	}
	if err != nil {
		_ = stream.Close()
		return nil, nil, err
	}
	return stream, frame, nil
}

func (s *ConnectorService) readClientReport(ctx context.Context, session tcp.Session, frame *protocol.Frame, name string, established bool) (protocol.ClientInfo, error) {
	payload, err := protocol.ReadPayload(frame)
	if err != nil {
		return protocol.ClientInfo{}, err
	}
	req, err := protocol.ParseConfigReportRequest(payload)
	if err != nil {
		return protocol.ClientInfo{}, err
	}
	conn, ok := tcp.Connection(session).(*secure.Conn)
	if !ok {
		return protocol.ClientInfo{}, ErrAuthRejected
	}
	ctx, cancel := context.WithTimeout(ctx, AuthTimeout)
	defer cancel()
	key, err := s.lookupTransportKey(ctx, conn.KeyID())
	if err != nil {
		return protocol.ClientInfo{}, ErrAuthRejected
	}
	if req.Info.Name == transport.LocalClientName || (established && req.Info.Name != name) {
		return protocol.ClientInfo{}, fmt.Errorf("%w: invalid client name", ErrAuthRejected)
	}
	if err := s.verifyKey(ctx, req.Info.Name, base64.StdEncoding.EncodeToString(key[:])); err != nil {
		return protocol.ClientInfo{}, fmt.Errorf("%w: %v", ErrAuthRejected, err)
	}
	return req.Info, nil
}

// configSnapshot 返回该客户端当前应下发的分层配置快照。
func (s *ConnectorService) configSnapshot(ctx context.Context, name string) protocol.PushConfig {
	if s.sessionCfg == nil {
		return protocol.PushConfig{}
	}
	return s.sessionCfg.ClientRuntime(ctx, name).Push
}

func (s *ConnectorService) saveClientReport(ctx context.Context, client *transport.Session, info protocol.ClientInfo) {
	client.UpdateInfo(info)
	if s.sessionCfg != nil {
		if reported, err := json.Marshal(info); err == nil {
			if err := s.sessionCfg.SaveClientReportedConfig(ctx, info.Name, string(reported)); err != nil {
				logging.Warn("core: save reported client config failed", "client", info.Name, "err", err)
			}
		}
	}
}

func (s *ConnectorService) handleClientReport(info *tcp.SessionInfo, stream tcp.Stream) error {
	ctx := s.context()
	if ctx == nil {
		return ErrAuthRejected
	}
	if err := stream.SetDeadline(time.Now().Add(AuthTimeout)); err != nil {
		return err
	}
	frame, err := protocol.NewDecoder(stream).ReadFrame()
	if err == nil && frame.Type != protocol.ConfigReport {
		_ = frame.CloseBody()
		err = fmt.Errorf("%w: expected config report", protocol.ErrProtocol)
	}
	if err != nil {
		_ = info.Session.Close()
		return err
	}
	reported, err := s.readClientReport(ctx, info.Session, frame, info.Name, true)
	if err != nil {
		protocol.WriteReply(stream, protocol.ConfigReportResult, protocol.Reply{Reason: err.Error()})
		_ = info.Session.Close()
		return err
	}
	client := clientOf(info)
	s.saveClientReport(ctx, client, reported)
	return client.SendConfigReportResult(stream, func() protocol.PushConfig { return s.configSnapshot(ctx, info.Name) })
}

// verifyKey 校验客户端认证。密钥表已配置（管理台可管）时按范围 key 判定：客户端名
// 落在某条记录的范围里且密钥相等才放行；表为空时回退配置文件的随机密钥，无密钥则拒绝。
func (s *ConnectorService) verifyKey(ctx context.Context, clientName, key string) error {
	if s.sessionCfg != nil {
		configured, ok := s.sessionCfg.MatchClientAuthKey(ctx, clientName, key)
		if configured {
			if !ok {
				return errKeyMismatch
			}
			return nil
		}
	}
	if _, err := authkey.Parse(s.authKey); err != nil {
		return errKeyMismatch
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(s.authKey)) != 1 {
		return errKeyMismatch
	}
	return nil
}
