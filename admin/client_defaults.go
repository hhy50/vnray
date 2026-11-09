package admin

import "github.com/hhy50/vnray/internal/config"

func normalizeGlobalClient(settings config.GlobalClientSettings) (config.GlobalClientSettings, error) {
	domains, ips := []string{}, []string{}
	for _, h := range settings.CustomHosts {
		domains = append(domains, h.Domain)
		ips = append(ips, h.IP)
	}
	hosts, err := parseCustomHosts(domains, ips)
	if err != nil {
		return config.GlobalClientSettings{}, err
	}
	froms, tos := []string{}, []string{}
	for _, m := range settings.IPMaps {
		froms = append(froms, m.From)
		tos = append(tos, m.To)
	}
	maps, err := parseIPMaps(froms, tos)
	if err != nil {
		return config.GlobalClientSettings{}, err
	}
	out := config.GlobalClientSettings{}
	for _, h := range hosts {
		out.CustomHosts = append(out.CustomHosts, config.CustomHost{Domain: h.Domain, IP: h.IP})
	}
	for _, m := range maps {
		out.IPMaps = append(out.IPMaps, config.IPMap{From: m.From, To: m.To})
	}
	return out, out.Validate()
}
