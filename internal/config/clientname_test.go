package config

import "testing"

// TestValidateClientName 覆盖 namespace:group:name 三段格式的合法与非法输入。
func TestValidateClientName(t *testing.T) {
	for _, tt := range []struct {
		name  string
		valid bool
	}{
		{"default:default:host-192.168.0.2", true},
		{"team1:laptop:x", true},
		{"a:b:c", true},
		{"edge-1", false},    // 旧的单段名字不再合法
		{"a:b", false},       // 少一段
		{"a:b:c:d", false},   // 多一段
		{"a::c", false},      // 段为空
		{":b:c", false},      // namespace 为空
		{"a:b:", false},      // name 为空
		{"team*:g:c", false}, // 通配符与服务端范围 key 语义冲突
		{"a:b:c*", false},    // 通配符在尾段
	} {
		err := ValidateClientName(tt.name)
		if (err == nil) != tt.valid {
			t.Errorf("ValidateClientName(%q) = %v, want valid=%v", tt.name, err, tt.valid)
		}
	}
}

// TestClientValidateNameFormat 校验 Client.Validate 会拦截非三段格式的 name，
// 留空则继续走默认名路径。
func TestClientValidateNameFormat(t *testing.T) {
	c := DefaultClient()
	c.Name = "edge-1"
	if err := c.Validate(); err == nil {
		t.Error("Validate with legacy one-segment name: want error")
	}
	c.Name = "default:default:edge-1"
	if err := c.Validate(); err != nil {
		t.Errorf("Validate with full name: %v", err)
	}
}
