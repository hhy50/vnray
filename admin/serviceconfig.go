package admin

// serviceconfig.go 只有运行期服务配置的接口与解析工具：读写口在 api_ops.go。

import (
	"fmt"
	"strings"
	"time"

	"github.com/hhy50/vnray/internal/config"
)

// ServiceSettings 是运行时服务配置的读写接口，由服务端装配层实现（core.serviceConfig），
// 经 AdminOptions.Settings 注入。admin 只依赖接口，不反向依赖 core。
type ServiceSettings interface {
	// Current 返回当前生效的配置快照。
	Current() config.ServiceSettings
	// Apply 校验并落库新配置，成功后热分发到运行态。
	Apply(config.ServiceSettings) error
}

// currentServiceSettings 返回当前生效的运行时配置；未注入 Settings（测试或未接装配）时
// 退回"默认值 + opts.PublicHost"，保证接口与分享链接仍可用。
func (s *AdminService) currentServiceSettings() config.ServiceSettings {
	if s.opts.Settings != nil {
		return s.opts.Settings.Current()
	}
	cur := config.DefaultServiceSettings()
	cur.PublicHost = s.opts.PublicHost
	return cur
}

// parseFormDuration 解析接口里的时长文本；空串保留 fallback，格式与取值范围由 Validate 兜底。
func parseFormDuration(field, raw string, fallback config.Duration) (config.Duration, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q (want e.g. 5s)", field, raw)
	}
	return config.Duration(d), nil
}
