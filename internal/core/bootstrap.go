package core

import (
	"context"
	"fmt"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/inbound"
	"github.com/hhy50/vnray/internal/rule"
	"github.com/hhy50/vnray/internal/sqliteweb"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/vless"
)

const initialClientAuthKey = "2nC7NBBcutpkJg0cPkyQgkzXqtWrri/JB4QyXI5N258="

func newBindingStore(ctx context.Context, cfg *config.Server) (store.Store, error) {
	data, err := initialDatabaseData(cfg)
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, cfg.Storage.ResolvedDriver(), cfg.Storage.DSN, store.WithInitialData(data))
}

func initialDatabaseData(cfg *config.Server) (store.InitialData, error) {
	settings, err := config.EncodeServiceSettings(defaultServiceSettings(cfg))
	if err != nil {
		return store.InitialData{}, err
	}
	vless, err := initialVLESSTLS()
	if err != nil {
		return store.InitialData{}, err
	}
	settings[config.SettingVLESS] = vless
	rules := map[string]string{}
	for name, r := range rule.Defaults() {
		raw, err := rule.Encode(r)
		if err != nil {
			return store.InitialData{}, err
		}
		rules[rule.Key(name)] = raw
	}
	plugins, err := initialPluginStates(cfg.Plugin.Enabled)
	if err != nil {
		return store.InitialData{}, err
	}
	return store.InitialData{
		AuthKey:  store.AuthKey{Key: initialClientAuthKey, Scope: "default:*"},
		Settings: settings, Rules: rules, Plugins: plugins,
	}, nil
}

// initialVLESSTLS 为全新数据库签发 vless 自签证书并编码成落库 JSON：新库的 vless
// 以 TLS 起步、指纹随库固定。初始化标记保证只在首次建库执行，既有明文库不受影响。
func initialVLESSTLS() (string, error) {
	cert, err := vless.Load("", nil, nil)
	if err != nil {
		return "", fmt.Errorf("seed vless tls: %w", err)
	}
	return config.EncodeVLESSSettings(config.VLESSSettings{TLS: &config.VLESSTLS{
		Enabled:    true,
		ServerName: cert.ServerName,
		CertPEM:    string(cert.CertPEM),
		KeyPEM:     string(cert.KeyPEM),
	}})
}

// initialPluginStates 合并各插件包给出的首次初始化记录：inbound 包只认识自己实现的
// 插件，sqlite-web 已独立成包，它那一条由装配层补上。
func initialPluginStates(enabled []string) ([]store.PluginState, error) {
	builtin := make([]string, 0, len(enabled))
	hasWebDB := false
	for _, name := range enabled {
		if name == sqliteweb.Name {
			hasWebDB = true
			continue
		}
		builtin = append(builtin, name)
	}
	states, err := inbound.InitialStates(builtin)
	if err != nil {
		return nil, err
	}
	if hasWebDB {
		states = append(states, sqliteweb.InitialState())
	}
	return states, nil
}
