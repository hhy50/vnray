package core

import (
	"context"
	"net"

	"github.com/hhy50/vnray/internal/authkey"
	"github.com/hhy50/vnray/internal/secure"
)

func (s *ConnectorService) upgradeConnection(ctx context.Context, raw net.Conn) (net.Conn, error) {
	return secure.Server(ctx, raw, s.lookupTransportKey, s.transportEncryption.Load())
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
