package admin

// 用户页「出口客户端」下拉里，本机出口（保留名 server:self）不是常驻项：它只来自
// client_bindings 里由 server_proxy 开关同步出来的那行记录。停用本机出口时该行被删，
// 下拉里也就选不到——这是「出口选择本机」最容易让人困惑的地方，先固定住现状。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/transport"
)

// zhRequest 是不带 Accept-Language 的请求：文案按默认语言（中文）输出。
func zhRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/admin/ops/api/users", nil)
}

// localOptionLabel 取出下拉里本机出口那一项；不存在时 ok 为 false。
func localOptionLabel(opts []apiClientOption) (string, bool) {
	for _, o := range opts {
		if o.Name == transport.LocalClientName {
			return o.Label, true
		}
	}
	return "", false
}

// TestClientOptionsLocalExit 断言本机出口的可见性完全由注册行决定：未登记时下拉里
// 没有这一项（用户无从选择），登记后带「本机」标注。
func TestClientOptionsLocalExit(t *testing.T) {
	s := newTestAdminService(t)
	s.opts.Bindings = NewBindings(s.opts.Store)

	if _, ok := localOptionLabel(s.clientOptions(zhRequest())); ok {
		t.Fatal("未登记 server:self 时不该出现本机出口选项")
	}

	if err := s.opts.Bindings.SetClient(ClientBinding{
		ClientKey:  transport.LocalClientName,
		ClientType: store.ClientTypeLocal,
	}); err != nil {
		t.Fatalf("register local exit: %v", err)
	}
	label, ok := localOptionLabel(s.clientOptions(zhRequest()))
	if !ok {
		t.Fatal("登记后应出现本机出口选项")
	}
	if !strings.Contains(label, "本机") {
		t.Fatalf("本机出口的标注 = %q, want 含「本机」", label)
	}
}

// TestPortMapExcludesLocalExit 断言端口映射的候选里没有本机出口：映射需要一个真实
// 会话承载，本机出口不参与，混进去只会在保存后被判无效。
func TestPortMapExcludesLocalExit(t *testing.T) {
	s := newTestAdminService(t)
	s.opts.Bindings = NewBindings(s.opts.Store)
	if err := s.opts.Bindings.SetClient(ClientBinding{
		ClientKey:  transport.LocalClientName,
		ClientType: store.ClientTypeLocal,
	}); err != nil {
		t.Fatalf("register local exit: %v", err)
	}
	if _, ok := localOptionLabel(s.clientOptionsForPortMap(zhRequest())); ok {
		t.Fatal("端口映射的候选不该包含本机出口")
	}
}
