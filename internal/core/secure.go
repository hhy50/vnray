package core

import (
	"context"
	"encoding/base64"
	"net"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/secure"
)

func (s *ConnectorService) upgradeConnection(ctx context.Context, raw net.Conn) (net.Conn, error) {
	conn, err := secure.Server(ctx, raw, s.lookupTransportKey, s.transportEncryption.Load())
	if err != nil {
		return nil, err
	}
	// 与客户端的握手日志配对，两端日志对得上才能排查模式不一致。
	id := conn.KeyID()
	logging.Info("server: handshake succeeded",
		"remote", conn.RemoteAddr().String(),
		"key_id", base64.StdEncoding.EncodeToString(id[:]),
		"encrypted", conn.Encrypted())
	return conn, nil
}

func (s *ConnectorService) lookupTransportKey(ctx context.Context, id authkey.ID) (authkey.Key, error) {
	if s.sessionCfg != nil {
		key, configured, err := s.sessionCfg.LookupTransportKey(ctx, id)
		if err != nil || configured {
			return key, err
		}
	}
	key, err := authkey.Parse(s.authKey)
	if err != nil || key.ID() != id {
		return authkey.Key{}, secure.ErrHandshake
	}
	return key, nil
}
