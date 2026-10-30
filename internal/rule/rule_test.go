package rule

import (
	"reflect"
	"testing"
)

// TestVerdict 断言名单判定：reject 优先于 direct，域名/IP/CIDR/通配各走各的路径。
func TestVerdict(t *testing.T) {
	c, err := Compile("t", Rule{
		Reject: []string{"*.blocked.example", "10.0.0.0/8"},
		Direct: []string{"example.com", "*.direct.example", "192.168.1.1"},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	cases := []struct {
		host string
		want Verdict
	}{
		{"api.blocked.example", Reject},
		{"blocked.example", Pass}, // 后缀通配要求前面还有标签
		{"10.1.2.3", Reject},
		{"example.com", Direct},
		{"EXAMPLE.com", Direct},
		{"a.b.direct.example", Direct},
		{"192.168.1.1", Direct},
		{"other.example", Pass},
	}
	for _, tc := range cases {
		if got := c.Verdict(tc.host); got != tc.want {
			t.Fatalf("Verdict(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// TestRejectWins 断言同一目标同时命中两个名单时拒绝优先。
func TestRejectWins(t *testing.T) {
	c, err := Compile("t", Rule{Reject: []string{"a.example"}, Direct: []string{"a.example"}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got := c.Verdict("a.example"); got != Reject {
		t.Fatalf("Verdict = %v, want reject", got)
	}
}

// TestAllPatternMatchesEverything 断言 "*" 匹配一切（想"全部直连"就建一条 direct: ["*"]）。
func TestAllPatternMatchesEverything(t *testing.T) {
	all, err := Compile("all", Rule{Direct: []string{"*"}})
	if err != nil {
		t.Fatalf("compile all: %v", err)
	}
	for _, host := range []string{"example.com", "1.1.1.1", "a.b"} {
		if got := all.Verdict(host); got != Direct {
			t.Fatalf("Verdict(%q) = %v, want direct", host, got)
		}
	}
}

// TestDefaultsOnlyContainDefault 断言预置规则集只有 default，且它是空名单。
func TestDefaultsOnlyContainDefault(t *testing.T) {
	defaults := Defaults()
	if len(defaults) != 1 {
		t.Fatalf("defaults = %v, want only default", defaults)
	}
	def, err := Compile(NameDefault, defaults[NameDefault])
	if err != nil {
		t.Fatalf("compile default: %v", err)
	}
	if got := def.Verdict("example.com"); got != Pass {
		t.Fatalf("default Verdict = %v, want pass", got)
	}
}

// TestCompileRejectsBadInput 断言非法条目与非法规则名在编译期就报错。
func TestCompileRejectsBadInput(t *testing.T) {
	if _, err := Compile("t", Rule{Reject: []string{"10.0.0.0/33"}}); err == nil {
		t.Fatal("want error for invalid CIDR")
	}
	if _, err := Compile("bad name", Rule{}); err == nil {
		t.Fatal("want error for invalid rule name")
	}
	if _, err := Compile("", Rule{}); err == nil {
		t.Fatal("want error for empty rule name")
	}
}

// TestEncodeRoundTrip 断言落库值与解析：空白项被丢弃，空名单序列化成 []。
func TestEncodeRoundTrip(t *testing.T) {
	raw, err := Encode(Rule{Reject: []string{" a.example ", ""}, Direct: nil})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if raw != `{"reject":["a.example"],"direct":[]}` {
		t.Fatalf("encode = %s", raw)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := (Rule{Reject: []string{"a.example"}, Direct: []string{}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("parse = %#v, want %#v", got, want)
	}
	if empty, err := Parse(""); err != nil || !reflect.DeepEqual(empty, Rule{}) {
		t.Fatalf("parse empty = %#v, %v", empty, err)
	}
}

// TestKeyRoundTrip 断言配置表键与规则名互转。
func TestKeyRoundTrip(t *testing.T) {
	if got := Key("cn"); got != "rule.cn" {
		t.Fatalf("Key = %q", got)
	}
	if name, ok := NameFromKey("rule.cn"); !ok || name != "cn" {
		t.Fatalf("NameFromKey = %q, %v", name, ok)
	}
	if _, ok := NameFromKey("log_level"); ok {
		t.Fatal("log_level should not parse as rule key")
	}
	if _, ok := NameFromKey("rule."); ok {
		t.Fatal("empty rule name should not parse")
	}
}
