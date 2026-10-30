package admin

// i18n.go 集中管理接口返回给前端的文案：handler 只写文案键，语言由请求的
// Accept-Language 决定。界面语言与错误文案必须一致，否则英文界面里会夹中文报错。

import (
	"fmt"
	"net/http"
	"strings"
)

// langID 是接口文案的语言标识。
type langID int

const (
	langZH langID = iota
	langEN
)

// msgKey 是文案键。用命名类型而不是裸字符串，避免把文案误传进键参数。
type msgKey string

// message 是同一条文案的中英两版。
type message struct {
	zh string
	en string
}

// 文案键按域分组；命名沿用「域+含义」，便于在 handler 里快速定位。
const (
	msgBadJSON          msgKey = "bad_json"
	msgAuthRequired     msgKey = "auth_required"
	msgCSRFInvalid      msgKey = "csrf_invalid"
	msgBadCredentials   msgKey = "bad_credentials"
	msgTooManyAttempts  msgKey = "too_many_attempts"
	msgSessionFailed    msgKey = "session_failed"
	msgCSRFFailed       msgKey = "csrf_failed"
	msgSaveFailedDetail msgKey = "save_failed_detail"
	msgOpFailedDetail   msgKey = "op_failed_detail"
	msgSaveStoreFailed  msgKey = "save_store_failed"
	msgDeleteStoreFail  msgKey = "delete_store_failed"
	msgReadStoreFailed  msgKey = "read_store_failed"
	msgNotConfigured    msgKey = "not_configured"
	msgLimitNegative    msgKey = "limit_negative"
	msgConcurrencyNeg   msgKey = "concurrency_negative"
	msgEnabledNotBool   msgKey = "enabled_not_bool"

	msgPortMapNotConfigured msgKey = "portmap_not_configured"
	msgPortMapNeedClient    msgKey = "portmap_need_client_host"
	msgPortOutOfRange       msgKey = "port_out_of_range"
	msgPortMapBadID         msgKey = "portmap_bad_id"
	msgPortMapNotFound      msgKey = "portmap_not_found"
	msgTLSNeedCertKey       msgKey = "tls_need_cert_key"
	msgTLSCertInvalid       msgKey = "tls_cert_invalid"
	msgCertKeyTogether      msgKey = "cert_key_together"

	msgPluginsNotConfigured msgKey = "plugins_not_configured"
	msgPluginNameEmpty      msgKey = "plugin_name_empty"
	msgPluginEnableFailed   msgKey = "plugin_enable_failed"
	msgPluginDisableFailed  msgKey = "plugin_disable_failed"
	msgPluginPortFailed     msgKey = "plugin_port_failed"
	msgPluginPortUnsupport  msgKey = "plugin_port_unsupported"
	msgPluginPortInvalid    msgKey = "plugin_port_invalid"
	msgPluginTLSUnsupported msgKey = "plugin_tls_unsupported"
	msgPluginTLSFailed      msgKey = "plugin_tls_failed"
	msgUnsupportedAction    msgKey = "unsupported_action"

	msgKeysNotConfigured msgKey = "keys_not_configured"
	msgKeyEmpty          msgKey = "key_empty"
	msgRuntimeMissing    msgKey = "runtime_config_missing"
	msgBadLogCursor      msgKey = "bad_log_cursor"

	msgBindingsNotConfigured msgKey = "bindings_not_configured"
	msgClientNameEmpty       msgKey = "client_name_empty"
	msgClientIDInvalid       msgKey = "client_id_invalid"
	msgClientNotOnline       msgKey = "client_not_online"
	msgKickFailed            msgKey = "kick_failed"
	msgConfigNotObject       msgKey = "config_not_object"
	msgServerConfigNotObject msgKey = "server_config_not_object"
	msgExistingConfigInvalid msgKey = "existing_config_invalid"

	msgProtocolKeyRequired msgKey = "protocol_key_required"
	msgNameClientRequired  msgKey = "name_client_required"
	msgSSMethodOnlyForSS   msgKey = "ss_method_only_for_ss"
	msgSSMethodUnsupported msgKey = "ss_method_unsupported"
	msgUserNotRegistered   msgKey = "user_not_registered"

	msgClientLabelOnline     msgKey = "client_label_online"
	msgClientLabelRegistered msgKey = "client_label_registered"
	msgClientLabelLocal      msgKey = "client_label_local"
	msgNoLogFile             msgKey = "no_log_file"

	msgRulesNotConfigured msgKey = "rules_not_configured"
	msgRuleNameInvalid    msgKey = "rule_name_invalid"
	msgRuleUnknown        msgKey = "rule_unknown"
)

// messages 是文案表：新增文案时中英两版必须同时补齐。
var messages = map[msgKey]message{
	msgBadJSON:          {"请求体不是合法 JSON: %v", "invalid JSON body: %v"},
	msgAuthRequired:     {"需要登录", "authentication required"},
	msgCSRFInvalid:      {"CSRF 令牌无效", "invalid CSRF token"},
	msgBadCredentials:   {"用户名或密码错误", "invalid username or password"},
	msgTooManyAttempts:  {"尝试次数过多，请 %d 分钟后再试", "too many attempts, try again in %d minutes"},
	msgSessionFailed:    {"生成会话失败", "failed to create session"},
	msgCSRFFailed:       {"生成 CSRF 令牌失败", "failed to create CSRF token"},
	msgSaveFailedDetail: {"保存失败: %v", "save failed: %v"},
	msgOpFailedDetail:   {"操作失败: %v", "operation failed: %v"},
	msgSaveStoreFailed:  {"保存失败：存储错误", "save failed: storage error"},
	msgDeleteStoreFail:  {"删除失败：存储错误", "delete failed: storage error"},
	msgReadStoreFailed:  {"读取失败：存储错误", "read failed: storage error"},
	msgNotConfigured:    {"运行时配置未接入", "runtime configuration is not wired up"},
	msgLimitNegative:    {"限速必须是非负整数", "rate limit must be a non-negative integer"},
	msgConcurrencyNeg:   {"并发上限必须是非负整数", "concurrency limit must be a non-negative integer"},
	msgEnabledNotBool:   {"enabled 必须是 true 或 false", "enabled must be true or false"},

	msgPortMapNotConfigured: {"端口映射未配置", "port mapping store is not configured"},
	msgPortMapNeedClient:    {"客户端与本地主机都必须填写", "client and local host are both required"},
	msgPortOutOfRange:       {"端口必须在 1-65535 之间", "port must be within 1-65535"},
	msgPortMapBadID:         {"映射编号不合法", "invalid mapping id"},
	msgPortMapNotFound:      {"没有这条映射", "mapping not found"},
	msgTLSNeedCertKey:       {"启用 HTTPS 时必须同时填写证书和私钥", "certificate and private key are both required to enable HTTPS"},
	msgTLSCertInvalid:       {"证书或私钥无效: %v", "invalid certificate or private key: %v"},
	msgCertKeyTogether:      {"证书和私钥必须同时填写", "certificate and private key must be provided together"},

	msgPluginsNotConfigured: {"插件管理器未配置", "plugin manager is not configured"},
	msgPluginNameEmpty:      {"插件名不能为空", "plugin name must not be empty"},
	msgPluginEnableFailed:   {"启用失败: %v", "enable failed: %v"},
	msgPluginDisableFailed:  {"停用失败：存储错误", "disable failed: storage error"},
	msgPluginPortFailed:     {"改端口失败: %v", "changing the port failed: %v"},
	msgPluginPortUnsupport:  {"该插件不支持运行中改端口", "this plugin does not support changing the port while running"},
	msgPluginPortInvalid:    {"端口必须是 1..65535 的整数", "port must be an integer within 1..65535"},
	msgPluginTLSUnsupported: {"该插件不支持 TLS", "this plugin does not support TLS"},
	msgPluginTLSFailed:      {"配置 TLS 失败: %v", "configuring TLS failed: %v"},
	msgUnsupportedAction:    {"不支持的动作", "unsupported action"},

	msgKeysNotConfigured: {"认证密钥未配置", "auth key store is not configured"},
	msgKeyEmpty:          {"key 不能为空", "key must not be empty"},
	msgRuntimeMissing:    {"运行时配置未接入", "runtime configuration is not wired up"},
	msgBadLogCursor:      {"日志游标无效", "invalid log cursor"},

	msgBindingsNotConfigured: {"转发绑定表未配置", "binding table is not configured"},
	msgClientNameEmpty:       {"客户端名称不能为空", "client name must not be empty"},
	msgClientIDInvalid:       {"客户端编号不合法", "invalid client id"},
	msgClientNotOnline:       {"没有这个在线客户端", "no such online client"},
	msgKickFailed:            {"断开失败: %v", "disconnect failed: %v"},
	msgConfigNotObject:       {"config 必须是 JSON 对象", "config must be a JSON object"},
	msgServerConfigNotObject: {"server_config 不是合法 JSON 对象", "server_config is not a valid JSON object"},
	msgExistingConfigInvalid: {"现有 server_config 不是合法 JSON，请先在配置页修正", "the stored server_config is not valid JSON; fix it on the settings page first"},

	msgProtocolKeyRequired: {"协议与 user_key 都必须填写", "protocol and user_key are both required"},
	msgNameClientRequired:  {"名称与出口客户端都必须填写", "name and egress client are both required"},
	msgSSMethodOnlyForSS:   {"只有 SS 用户可以指定加密类型", "only SS users may specify a cipher"},
	msgSSMethodUnsupported: {"不支持的 SS 加密类型", "unsupported SS cipher"},
	msgUserNotRegistered:   {"用户未登记", "user not registered"},

	msgClientLabelOnline:     {"%s（在线）", "%s (online)"},
	msgClientLabelRegistered: {"%s（已注册）", "%s (registered)"},
	msgClientLabelLocal:      {"%s（本机）", "%s (this host)"},
	msgNoLogFile: {
		"日志输出到 stderr，未写文件；在配置 log.output 设置文件路径后可在此查看。",
		"Logs go to stderr and are not written to a file; set log.output to a file path to read them here.",
	},

	msgRulesNotConfigured: {"出站规则未接入", "outbound rules are not wired up"},
	msgRuleNameInvalid:    {"规则名不合法: %v", "invalid rule name: %v"},
	msgRuleUnknown:        {"没有这条出站规则: %s", "no such outbound rule: %s"},
}

// requestLang 按 Accept-Language 选语言：浏览器已按 q 值排序，这里取首个认识的
// 语言标签即可；只有 zh 与 en 两套文案，其余一律回落中文。
func requestLang(r *http.Request) langID {
	if r == nil {
		return langZH
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		switch {
		case strings.HasPrefix(tag, "zh"):
			return langZH
		case strings.HasPrefix(tag, "en"):
			return langEN
		}
	}
	return langZH
}

// msg 返回按请求语言本地化的文案，args 交给 fmt.Sprintf 填充占位符。
func msg(r *http.Request, key msgKey, args ...any) string {
	m, ok := messages[key]
	if !ok {
		// 缺文案属于编码错误：返回键名，界面上一眼能看出漏配，不要静默成空串。
		return string(key)
	}
	text := m.zh
	if requestLang(r) == langEN {
		text = m.en
	}
	if len(args) > 0 {
		return fmt.Sprintf(text, args...)
	}
	return text
}

// apiFailKey 是按文案键输出错误的 apiFail。
func apiFailKey(w http.ResponseWriter, r *http.Request, code int, key msgKey, args ...any) {
	apiFail(w, code, msg(r, key, args...))
}
