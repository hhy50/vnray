package admin

import (
	"encoding/base64"
	"net"
	"net/url"
	"strconv"

	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/shadowsocks"
)

func ssLink(target shareTarget, method, password, name string) string {
	m, err := shadowsocks.ParseMethod(method)
	if err != nil || target.Port <= 0 || target.Host == "" {
		return ""
	}
	if name == "" {
		name = "vnray-ss"
	}
	credentials := base64.RawURLEncoding.EncodeToString([]byte(m.Name + ":" + password))
	return "ss://" + credentials + "@" + net.JoinHostPort(target.Host, strconv.Itoa(target.Port)) +
		"#" + url.PathEscape(name)
}

func (s *AdminService) ssShareSettings() (int, string) {
	method := plugin.DefaultSSMethod
	if s.opts.Plugins == nil {
		return 0, method
	}
	p, err := s.opts.Plugins.Get("ss")
	if err != nil {
		return 0, method
	}
	status := p.Status()
	if m, err := shadowsocks.ParseMethod(status.Params["method"]); err == nil {
		method = m.Name
	}
	for _, port := range status.Ports {
		if port.Protocol == store.ProtocolSS {
			return port.Port, method
		}
	}
	return 0, method
}
