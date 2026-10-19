package protocol

import (
	"encoding/json"
	"fmt"
	"io"
)

// ClientInfo 是客户端随认证上报的自身配置与环境摘要（Auth 帧载荷的一部分）。
type ClientInfo struct {
	Name     string `json:"name,omitempty"`
	Version  string `json:"version,omitempty"`
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Server   string `json:"server,omitempty"`
	Public   string `json:"public,omitempty"`
	Private  string `json:"private,omitempty"`
	// Interfaces 是客户端本机的网卡快照，供服务端判断它从哪条链路接进来。
	Interfaces []NetInterface `json:"interfaces,omitempty"`
	// 以下三项是生效配置摘要，字段名与客户端 YAML 配置对齐；nil 表示未配置该项。
	Log       *ClientLog       `json:"log,omitempty"`
	Reconnect *ClientReconnect `json:"reconnect,omitempty"`
	Routing   *ClientRouting   `json:"routing,omitempty"`
}

// NetInterface 是一块网卡的摘要：只保留诊断用得上的名称、MTU、MAC、状态与地址。
type NetInterface struct {
	Name  string   `json:"name"`
	MTU   int      `json:"mtu,omitempty"`
	MAC   string   `json:"mac,omitempty"`
	Flags string   `json:"flags,omitempty"`
	Addrs []string `json:"addrs,omitempty"`
}

// ClientLog 是客户端生效的日志配置摘要。
type ClientLog struct {
	Level   string `json:"level,omitempty"`
	Output  string `json:"output,omitempty"`
	Console bool   `json:"console,omitempty"`
}

// ClientReconnect 是客户端生效的重连配置摘要。
type ClientReconnect struct {
	Enabled  bool   `json:"enabled"`
	Interval string `json:"interval,omitempty"`
}

// ClientRouting 是客户端生效的路由配置摘要。
type ClientRouting struct {
	Default   string           `json:"default,omitempty"`
	Outbounds []ClientOutbound `json:"outbounds,omitempty"`
	Rules     []ClientRule     `json:"rules,omitempty"`
}

// ClientOutbound 是路由配置里的一个出站。
type ClientOutbound struct {
	Tag    string `json:"tag,omitempty"`
	Type   string `json:"type,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ClientRule 是路由配置里的一条规则。
type ClientRule struct {
	Name     string      `json:"name,omitempty"`
	Outbound string      `json:"outbound,omitempty"`
	Match    ClientMatch `json:"match"`
}

// ClientMatch 是规则的命中条件集合，各条件与各值之间均为 OR。
type ClientMatch struct {
	Host          []string `json:"host,omitempty"`
	DomainPrefix  []string `json:"domain_prefix,omitempty"`
	DomainSuffix  []string `json:"domain_suffix,omitempty"`
	DomainKeyword []string `json:"domain_keyword,omitempty"`
	Port          []int    `json:"port,omitempty"`
}

// AuthRequest 是 Auth 帧的载荷：共享密钥 + 客户端自报信息。
type AuthRequest struct {
	Key  string     `json:"key"`
	Info ClientInfo `json:"client"`
}

// UserInfo 是用户端随 UserAuth 上报的自身信息（UserAuth 帧载荷的一部分）。
type UserInfo struct {
	Name     string `json:"name,omitempty"`
	Version  string `json:"version,omitempty"`
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	PID      int    `json:"pid,omitempty"`
}

// UserAuthRequest 是 UserAuth 帧的载荷：用户密钥 + 用户端自报信息。通过后该会话成为
// 用户会话，后续 Forward 按管理员配置的 user_key → 客户端 绑定选路。
type UserAuthRequest struct {
	Key  string   `json:"key"`
	Info UserInfo `json:"user"`
}

// TunnelRequest 是 Tunnel 帧载荷里地址块之后的 JSON：这次隧道由谁发起，便于出口侧诊断；
// 目标在载荷开头的 Address 块里。
type TunnelRequest struct {
	Origin string `json:"origin,omitempty"`
}

// Reply 是各类结果帧（AuthResult / ForwardResult / TunnelResult）的载荷。
type Reply struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	// Client 仅用户认证成功时非空：绑定的客户端配置（在线为完整上报值，离线仅名字）。
	Client *ClientInfo `json:"client,omitempty"`
	// Config 仅客户端认证应答（AuthResult）里非空：服务端下发的全量配置快照，
	// 客户端重连时随认证一并拿到，保证重启不丢配置。
	Config *PushConfig `json:"config,omitempty"`
}

// CustomHost 是一条自定义域名映射：客户端出站拨号时命中 Domain 的域名改用 IP
// 直连，未命中才走 DNS 解析。空表即"全部走 DNS"。
type CustomHost struct {
	Domain string `json:"domain"`
	IP     string `json:"ip"`
}

// IPMap 是一条 IPv4 段映射：目标 IP 命中 From 模式时按 To 模式重写。
// 模式为点分四段，每段是 0-255 字面量或 * 通配（* 位取源 IP 的对应段），
// 如 123.123.*.* → 192.168.*.* 把 123.123.x.y 改写为 192.168.x.y。
type IPMap struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// PushConfig 是服务端下发给客户端的一次全量配置快照：当前有自定义 host 与 IP
// 段映射，后续新增配置项直接在这里加字段，两端同步升级即可，不必新增消息类型。
type PushConfig struct {
	// CustomHosts 为 nil/空时表示不启用自定义域名表，全部走 DNS。
	CustomHosts []CustomHost `json:"custom_hosts,omitempty"`
	// IPMaps 为 nil/空时表示不启用 IP 段映射。
	IPMaps []IPMap `json:"ip_maps,omitempty"`
}

// newJSONFrame 构造载荷为 v 的 JSON 帧。
func newJSONFrame(typ Type, v any) (*Frame, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode %v frame payload: %w", typ, err)
	}
	return NewBufferedFrame(typ, payload), nil
}

// NewAuthFrame 构造认证帧。
func NewAuthFrame(req AuthRequest) (*Frame, error) {
	return newJSONFrame(Auth, req)
}

// NewUserAuthFrame 构造用户认证帧。
func NewUserAuthFrame(req UserAuthRequest) (*Frame, error) {
	return newJSONFrame(UserAuth, req)
}

// NewForwardFrame 构造转发请求帧：载荷即地址块。
func NewForwardFrame(a Address) (*Frame, error) {
	payload, err := a.Bytes()
	if err != nil {
		return nil, err
	}
	return NewBufferedFrame(Forward, payload), nil
}

// NewTunnelFrame 构造隧道请求帧：载荷 = 地址块 + JSON 请求体。
func NewTunnelFrame(req TunnelRequest, a Address) (*Frame, error) {
	addr, err := a.Bytes()
	if err != nil {
		return nil, err
	}
	jsonPart, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode tunnel frame payload: %w", err)
	}
	return NewBufferedFrame(Tunnel, append(addr, jsonPart...)), nil
}

// ReadTunnelRequest 从 Tunnel 帧载荷读出地址块与请求体；读完整个载荷，
// 调用方随后须以 CloseBody 校验 CRC。
func ReadTunnelRequest(body io.Reader) (Address, TunnelRequest, error) {
	a, err := ReadAddress(body)
	if err != nil {
		return Address{}, TunnelRequest{}, err
	}
	rest, err := io.ReadAll(body)
	if err != nil {
		return Address{}, TunnelRequest{}, protocolError("read tunnel request: %v", err)
	}
	var req TunnelRequest
	if err := decodeJSON(rest, &req, "tunnel request"); err != nil {
		return Address{}, TunnelRequest{}, err
	}
	return a, req, nil
}

// NewPortMapFrame 构造端口映射请求帧：载荷即地址块（客户端侧的本地拨号目标）。
func NewPortMapFrame(a Address) (*Frame, error) {
	payload, err := a.Bytes()
	if err != nil {
		return nil, err
	}
	return NewBufferedFrame(PortMap, payload), nil
}

// NewReplyFrame 构造任一类结果帧（AuthResult / ForwardResult / TunnelResult）。
func NewReplyFrame(typ Type, res Reply) (*Frame, error) {
	switch typ {
	case AuthResult, ForwardResult, TunnelResult:
	default:
		return nil, fmt.Errorf("%w: %v is not a result frame type", ErrProtocol, typ)
	}
	return newJSONFrame(typ, res)
}

// NewPushConfigFrame 构造全量配置下发帧，载荷为 PushConfig JSON 快照。
func NewPushConfigFrame(cfg PushConfig) (*Frame, error) {
	return newJSONFrame(ConfigPush, cfg)
}

// ParsePushConfig 解析全量配置下发帧载荷。
func ParsePushConfig(payload []byte) (PushConfig, error) {
	var cfg PushConfig
	if err := decodeJSON(payload, &cfg, "push config"); err != nil {
		return PushConfig{}, err
	}
	return cfg, nil
}

// decodeJSON 解析帧载荷 JSON。
func decodeJSON(payload []byte, v any, what string) error {
	if err := json.Unmarshal(payload, v); err != nil {
		return fmt.Errorf("%w: decode %s: %v", ErrProtocol, what, err)
	}
	return nil
}

// ParseAuthRequest 解析认证帧载荷。
func ParseAuthRequest(payload []byte) (AuthRequest, error) {
	var req AuthRequest
	return req, decodeJSON(payload, &req, "auth request")
}

// ParseUserAuthRequest 解析用户认证帧载荷。
func ParseUserAuthRequest(payload []byte) (UserAuthRequest, error) {
	var req UserAuthRequest
	return req, decodeJSON(payload, &req, "user auth request")
}

// ParseTunnelRequest 解析隧道请求帧载荷。
func ParseTunnelRequest(payload []byte) (TunnelRequest, error) {
	var req TunnelRequest
	return req, decodeJSON(payload, &req, "tunnel request")
}

// ParseReply 解析结果帧载荷。
func ParseReply(payload []byte) (Reply, error) {
	var res Reply
	return res, decodeJSON(payload, &res, "reply")
}
