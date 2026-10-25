package admin

// vmesslink.go 生成 v2rayN/Xray 通用的 vmess:// 分享链接：链接内容只取决于
// 服务器地址、v2ray 入站端口与用户 uuid，与运行态无关，便于单测。

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hhy50/vnray/internal/store"
)

// vmessShare 是 vmess:// 的 JSON 载荷（v2rayN 第 2 版格式，各家客户端通用）。
type vmessShare struct {
	V    string `json:"v"`
	PS   string `json:"ps"`
	Add  string `json:"add"`
	Port string `json:"port"`
	ID   string `json:"id"`
	Aid  string `json:"aid"`
	Scy  string `json:"scy"`
	Net  string `json:"net"`
	Type string `json:"type"`
	Host string `json:"host"`
	Path string `json:"path"`
	TLS  string `json:"tls"`
}

// vmessLink 把一条 v2ray 用户记录编码成 vmess:// 链接。本服务的 vmess 入站是纯 TCP
// 上的 VMess AEAD（alterId=0，无 TLS/WS），传输相关字段因此全为空值。
func vmessLink(t shareTarget, uuid, name string) string {
	payload := vmessShare{
		V: "2", PS: name, Add: t.Host, Port: strconv.Itoa(t.Port),
		ID: uuid, Aid: "0", Scy: "auto", Net: "tcp", Type: "none",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(raw)
}

// shareName 生成链接里的节点名：uuid 前 8 位足够区分同一客户端下的多个用户。
func shareName(uuid string) string {
	if len(uuid) > 8 {
		uuid = uuid[:8]
	}
	return "vnray-" + uuid
}

// shareHost 返回链接里的服务器地址：优先取运行时配置的 public_host（管理台只监听回环
// 地址，请求 Host 通常就是 127.0.0.1，那种链接客户端用不了），否则退回请求的 Host。
func (s *AdminService) shareHost(r *http.Request) string {
	if h := bareHost(s.currentServiceSettings().PublicHost); h != "" {
		return h
	}
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		return h
	}
	return bareHost(r.Host)
}

// bareHost 把 "https://example.com:8443/path" 归一成 "example.com"：配置项写错
// 形态时链接仍然可用，而不是直接坏掉。
func bareHost(raw string) string {
	h := strings.TrimSpace(raw)
	if h == "" {
		return ""
	}
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.Trim(h, "[]")
}

// shareTLS 是分享链接的 TLS 参数；nil 表示明文入站。
type shareTLS struct {
	// ServerName 是链接里的 sni；空则退回分享主机。
	ServerName string
	// SelfSigned 表示证书自签，链接需带 allowInsecure=1，否则客户端直接握手失败。
	SelfSigned bool
}

// shareTarget 是一条分享链接的入站目标：服务器地址、端口与（vless 的）TLS 参数。
type shareTarget struct {
	Host string
	Port int
	TLS  *shareTLS
}

// vlessLink 把一条 vless 用户记录编码成 vless:// 链接（各家客户端通用）。TLS 参数
// 与 v2ray 客户端的默认值对齐：alpn=h2,http/1.1 是服务端 tls.Config 的默认 ALPN，
// fp=chrome 走 uTLS 指纹，自签证书另加 allowInsecure=1。
func vlessLink(t shareTarget, uuid, name string) string {
	q := url.Values{}
	q.Set("encryption", "none")
	q.Set("type", "tcp")
	if t.TLS == nil {
		q.Set("security", "none")
	} else {
		sni := t.TLS.ServerName
		if sni == "" {
			sni = t.Host
		}
		q.Set("security", "tls")
		q.Set("sni", sni)
		q.Set("fp", "chrome")
		q.Set("alpn", "h2,http/1.1")
		if t.TLS.SelfSigned {
			q.Set("allowInsecure", "1")
		}
	}
	return "vless://" + uuid + "@" + net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) +
		"?" + q.Encode() + "#" + url.QueryEscape(name)
}

// linkForPort 按协议生成分享链接；端口为 0（插件未启用或未配该协议）时返回空串。
func linkForPort(t shareTarget, uuid string, vmess bool) string {
	if t.Port <= 0 {
		return ""
	}
	name := shareName(uuid)
	if vmess {
		return vmessLink(t, uuid, name)
	}
	return vlessLink(t, uuid, name)
}

// v2rayPorts 返回 v2ray 插件两个入站的当前端口（vmess / vless）；插件未装配或该协议
// 没端口时返回 0。
func (s *AdminService) v2rayPorts() (vmessPort, vlessPort int) {
	if s.opts.Plugins == nil {
		return 0, 0
	}
	p, err := s.opts.Plugins.Get("v2ray")
	if err != nil {
		return 0, 0
	}
	for _, kv := range p.Status().Ports {
		switch kv.Protocol {
		case store.ProtocolVMess:
			vmessPort = kv.Port
		case store.ProtocolVLESS:
			vlessPort = kv.Port
		}
	}
	return vmessPort, vlessPort
}

// vlessShareTLS 返回 vless 入站的 TLS 链接参数；未启用 TLS 时返回 nil（链接走明文）。
func (s *AdminService) vlessShareTLS() *shareTLS {
	if s.opts.Plugins == nil {
		return nil
	}
	p, err := s.opts.Plugins.Get("v2ray")
	if err != nil {
		return nil
	}
	info := p.Status().TLS
	if info == nil || !info.Enabled {
		return nil
	}
	return &shareTLS{ServerName: info.ServerName, SelfSigned: info.SelfSigned}
}
