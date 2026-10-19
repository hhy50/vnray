package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultLogLevel  = "info"
	DefaultLogOutput = "stderr"
	// DefaultReconnectInterval 是两次重连之间的默认固定间隔。
	DefaultReconnectInterval = 5 * time.Second
)

// Duration 是 YAML 中的时长文本，如 "5s"、"500ms"。
// 它在**解码时**就按 time.ParseDuration 校验，配置写错在加载阶段即报错，
// 不会拖到运行期才暴露。
type Duration time.Duration

// UnmarshalYAML 解析时长文本。
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var text string
	if err := node.Decode(&text); err != nil {
		return fmt.Errorf("duration must be a string like \"5s\": %w", err)
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std 转换为标准库 time.Duration。
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String 返回可读文本，如 "5s"。
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON 以文本形态输出时长，与管理台保存的 JSON 快照一致。
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// UnmarshalJSON 解析文本时长，与 UnmarshalYAML 同一校验口径。
func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("duration must be a string like \"5s\": %w", err)
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	*d = Duration(parsed)
	return nil
}

var (
	validLevels   = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	validOutputs  = map[string]bool{"stderr": true, "stdout": true}
	validOutTypes = map[string]bool{"direct": true, "reject": true}
)

type Log struct {
	Level  string `yaml:"level"`
	Output string `yaml:"output"`
	// Console 为 true 且 Output 为文件路径时，日志同时镜像输出到终端。
	Console bool `yaml:"console"`
}

// DefaultLog 返回默认日志配置。
func DefaultLog() *Log {
	return &Log{Level: DefaultLogLevel, Output: DefaultLogOutput}
}

// Validate 校验日志字段。
func (l *Log) Validate() error {
	if !validLevels[l.Level] {
		return fmt.Errorf("log.level: %q is invalid, expect one of debug/info/warn/error", l.Level)
	}
	if l.Output == "" {
		return fmt.Errorf("log.output: must not be empty, expect stderr/stdout or a file path")
	}
	return nil
}

// IsStdOutput 判断 Output 是否为标准错误/标准输出。
func (l Log) IsStdOutput() bool { return validOutputs[l.Output] }

// Auth 是握手认证配置：两端用同一个共享密钥，客户端上报、服务端比对。
// 服务端 Key 为空表示不校验（启动时会告警）；设置后长度至少 8 个字符。
type Auth struct {
	Key string `yaml:"key"`
}

// Validate 校验认证配置；未启用（Key 为空）时不做要求。
func (a Auth) Validate() error {
	if a.Key != "" && len(a.Key) < 8 {
		return fmt.Errorf("auth.key: must be at least 8 characters when set")
	}
	return nil
}

// Reconnect 控制连接失败或会话中断后的重连行为：客户端默认应长期在线，
// 因此默认开启重连，靠固定间隔把断线自愈掉，而不是让进程退出。
type Reconnect struct {
	// Enabled 为 false 时拨号失败或会话结束即返回错误，由进程管理器拉起。
	Enabled bool `yaml:"enabled"`
	// Interval 是两次重连之间的固定等待。
	Interval Duration `yaml:"interval"`
}

// DefaultReconnect 返回内置默认重连策略：失败后每隔 5s 不断重连。
func DefaultReconnect() Reconnect {
	return Reconnect{Enabled: true, Interval: Duration(DefaultReconnectInterval)}
}

// Validate 校验重连配置；开启时必须给出正间隔，否则退化成忙等重连。
func (r Reconnect) Validate() error {
	if r.Enabled && r.Interval <= 0 {
		return fmt.Errorf("reconnect.interval: must be positive when reconnect is enabled")
	}
	return nil
}

// Load 读取 YAML 文件并解码进 dst；调用方应先把 dst 填成默认值，
// 未出现的键保留默认值，未知键会被拒绝。
func Load(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config %s: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	return nil
}

// validateAddress 校验 host:port 形式的地址（含带方括号的 IPv6）。
func validateAddress(field, address string) error {
	host, portText, err := splitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if host == "" {
		return fmt.Errorf("%s: host must not be empty", field)
	}
	if !validPortText(portText) {
		return fmt.Errorf("%s: port must be in 1..65535", field)
	}
	return nil
}
