package core

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
	"github.com/hhy50/vnray/internal/transport"
)

// AuthTimeout 是握手各阶段的等待上限（服务端等认证流、两端等对端帧）。
const AuthTimeout = 5 * time.Second

// ErrAuthRejected 表示服务端拒绝了这次认证。
var ErrAuthRejected = errors.New("authentication rejected")

// errKeyMismatch 是密钥不匹配的原因，会作为拒绝原因回给客户端。
var errKeyMismatch = errors.New("key mismatch")

// Handshake 实现 tcp.EventListener：在会话转入数据面之前完成认证，并按首帧区分角色——
// Auth（客户端）走共享密钥比对，UserAuth（用户）查绑定表得到其绑定的客户端名。
// 返回的元数据由 SessionManager 在**认证成功之后**登记；任一路径失败都返回零值，不入表。
func (s *ConnectorService) Handshake(session tcp.Session) (tcp.SessionInfo, error) {
	// AcceptStream 的 deadline 是会话级设置，握手期间独享，返回前必须清掉。
	if err := session.SetDeadline(time.Now().Add(AuthTimeout)); err != nil {
		return tcp.SessionInfo{}, fmt.Errorf("core: set auth accept deadline: %w", err)
	}
	defer func() { _ = session.SetDeadline(time.Time{}) }()

	stream, err := session.AcceptStream()
	if err != nil {
		return tcp.SessionInfo{}, fmt.Errorf("core: accept auth stream: %w", err)
	}
	defer func() { _ = stream.Close() }()
	if err := stream.SetDeadline(time.Now().Add(AuthTimeout)); err != nil {
		return tcp.SessionInfo{}, fmt.Errorf("core: set auth stream deadline: %w", err)
	}

	frame, err := protocol.NewDecoder(stream).ReadFrame()
	if err != nil {
		return tcp.SessionInfo{}, fmt.Errorf("core: read auth frame: %w", err)
	}
	switch frame.Type {
	case protocol.Auth:
		return s.handshakeClient(session, stream, frame)
	case protocol.UserAuth:
		return s.handshakeUser(session, stream, frame)
	default:
		_ = frame.CloseBody()
		return tcp.SessionInfo{}, fmt.Errorf("%w: unexpected frame %d on auth stream", protocol.ErrProtocol, frame.Type)
	}
}

// handshakeClient 校验客户端共享密钥；通过后返回会话元数据供 SessionManager 登记。
func (s *ConnectorService) handshakeClient(session tcp.Session, stream tcp.Stream, frame *protocol.Frame) (tcp.SessionInfo, error) {
	payload, err := protocol.ReadPayload(frame)
	if err != nil {
		return tcp.SessionInfo{}, err
	}
	req, err := protocol.ParseAuthRequest(payload)
	if err != nil {
		return tcp.SessionInfo{}, err
	}

	// 保留名归服务端本机出口：真实客户端不得冒用（防止它顶替本机出口出现在管理台）。
	if req.Info.Name == transport.LocalClientName {
		protocol.WriteReply(stream, protocol.AuthResult, protocol.Reply{OK: false, Reason: "reserved client name"})
		return tcp.SessionInfo{}, fmt.Errorf("%w: reserved client name %q", ErrAuthRejected, req.Info.Name)
	}

	if err := s.verifyKey(req.Info.Name, req.Key); err != nil {
		protocol.WriteReply(stream, protocol.AuthResult, protocol.Reply{OK: false, Reason: err.Error()})
		return tcp.SessionInfo{}, fmt.Errorf("%w: %v", ErrAuthRejected, err)
	}
	// 应答带上服务端下发的全量配置快照：客户端重连/重启时随认证一并拿到，不丢配置。
	var pushCfg protocol.PushConfig
	if s.sessionCfg != nil {
		pushCfg = s.sessionCfg.PushConfig(req.Info.Name)
	}
	protocol.WriteReply(stream, protocol.AuthResult, protocol.Reply{OK: true, Config: &pushCfg})
	// 会话日志器取客户端配置的级别：此后这条会话的日志都走它，不再用全局级别。
	var level string
	if s.sessionCfg != nil {
		level = s.sessionCfg.ClientLogLevel(req.Info.Name)
		// 上报快照持久化到 client_bindings.config：离线也能看最后一次上报。
		if reported, err := json.Marshal(req.Info); err == nil {
			if err := s.sessionCfg.SaveClientReportedConfig(req.Info.Name, string(reported)); err != nil {
				logging.Warn("core: save reported client config failed", "client", req.Info.Name, "err", err)
			}
		}
	}
	client := transport.NewSession(session, session.RemoteAddr().String(), req.Info)
	// 客户端级并发上限（对设备的 OpenStream）在会话建立时按运维配置设定。
	if s.sessionCfg != nil {
		client.SetClientConcurrency(s.sessionCfg.ClientConcurrency(req.Info.Name))
	}
	return tcp.SessionInfo{
		Session: session,
		Remote:  session.RemoteAddr().String(),
		Kind:    tcp.KindClient,
		Name:    req.Info.Name,
		Log:     logging.NewScoped(level, "client", req.Info.Name),
		Meta:    client,
	}, nil
}

// handshakeUser 校验用户密钥：必须已在绑定表中登记，通过后返回会话元数据。
// 用户会话不进管理台、不跑心跳，只按绑定的客户端名选路。
func (s *ConnectorService) handshakeUser(session tcp.Session, stream tcp.Stream, frame *protocol.Frame) (tcp.SessionInfo, error) {
	payload, err := protocol.ReadPayload(frame)
	if err != nil {
		return tcp.SessionInfo{}, err
	}
	req, err := protocol.ParseUserAuthRequest(payload)
	if err != nil {
		return tcp.SessionInfo{}, err
	}

	if s.bindings == nil {
		protocol.WriteReply(stream, protocol.AuthResult, protocol.Reply{OK: false, Reason: "user access is not enabled"})
		return tcp.SessionInfo{}, fmt.Errorf("%w: user access is not enabled", ErrAuthRejected)
	}
	access, ok := s.bindings.UserFor(req.Key)
	if !ok {
		protocol.WriteReply(stream, protocol.AuthResult, protocol.Reply{OK: false, Reason: "user key is not registered"})
		return tcp.SessionInfo{}, fmt.Errorf("%w: user key is not registered", ErrAuthRejected)
	}
	if !access.Active {
		// 停用的用户认得出是谁，但不放行；应答与日志都要能区分"停用"和"没登记"。
		protocol.WriteReply(stream, protocol.AuthResult, protocol.Reply{OK: false, Reason: "user is disabled"})
		return tcp.SessionInfo{}, fmt.Errorf("%w: user is disabled", ErrAuthRejected)
	}
	protocol.WriteReply(stream, protocol.AuthResult, s.userAuthReply(access.Client))
	// 会话日志器取该用户配置的级别；Meta 带上 UserMeta：转发桥按 user_key 从限速
	// 注册表取桶，用户上报的自信息随会话保留（客户端会话的 Meta 是 *transport.Session）。
	var level string
	if s.sessionCfg != nil {
		if cfg, ok := s.sessionCfg.UserConfig(store.ProtocolVNV1, req.Key); ok {
			level = cfg.LogLevel
		}
	}
	return tcp.SessionInfo{
		Session: session,
		Remote:  session.RemoteAddr().String(),
		Kind:    tcp.KindUser,
		Name:    access.Client,
		Log:     logging.NewScoped(level, "user", req.Key),
		Meta:    transport.UserMeta{Key: req.Key, Name: access.Name, Info: req.Info},
	}, nil
}

// userAuthReply 组装用户认证成功的应答：回带绑定客户端的配置——在线时是它上报的
// 完整配置，离线时仅剩名字（客户端的配置随会话存在，服务端不另存）。
func (s *ConnectorService) userAuthReply(clientName string) protocol.Reply {
	reply := protocol.Reply{OK: true, Client: &protocol.ClientInfo{Name: clientName}}
	if info, ok := s.sessions.SessionByName(clientName); ok {
		if client := clientOf(info); client != nil {
			reported := client.Info()
			reply.Client = &reported
		}
	}
	return reply
}

// verifyKey 校验客户端认证。密钥表已配置（管理台可管）时按范围 key 判定：客户端名
// 落在某条记录的范围里且密钥相等才放行；表为空时回退配置文件的共享密钥，两者都没
// 配就放行——维持未启用认证的部署行为。
func (s *ConnectorService) verifyKey(clientName, key string) error {
	if s.sessionCfg != nil {
		configured, ok := s.sessionCfg.MatchClientAuthKey(clientName, key)
		if configured {
			if !ok {
				return errKeyMismatch
			}
			return nil
		}
	}
	if s.authKey == "" {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(s.authKey)) != 1 {
		return errKeyMismatch
	}
	return nil
}
