package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestLang(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		accept string
		want   langID
	}{
		{name: "简体中文", accept: "zh-CN,zh;q=0.9,en;q=0.8", want: langZH},
		{name: "英文", accept: "en-US,en;q=0.9", want: langEN},
		{name: "英文优先", accept: "en;q=1.0,zh;q=0.5", want: langEN},
		{name: "未声明回落中文", accept: "", want: langZH},
		{name: "不认识的语言回落中文", accept: "ja-JP,ja;q=0.9", want: langZH},
		{name: "带空格", accept: " en-GB ", want: langEN},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/admin/ops/api/status", nil)
			if tc.accept != "" {
				r.Header.Set("Accept-Language", tc.accept)
			}
			if got := requestLang(r); got != tc.want {
				t.Fatalf("requestLang(%q) = %v, want %v", tc.accept, got, tc.want)
			}
		})
	}
}

func TestMsg(t *testing.T) {
	t.Parallel()

	en := httptest.NewRequest(http.MethodGet, "/", nil)
	en.Header.Set("Accept-Language", "en-US")
	zh := httptest.NewRequest(http.MethodGet, "/", nil)

	if got, want := msg(zh, msgPortOutOfRange), "端口必须在 1-65535 之间"; got != want {
		t.Fatalf("中文文案 = %q, want %q", got, want)
	}
	if got, want := msg(en, msgPortOutOfRange), "port must be within 1-65535"; got != want {
		t.Fatalf("英文文案 = %q, want %q", got, want)
	}

	// 占位符按参数填充，且两种语言的参数顺序各自独立。
	if got, want := msg(en, msgBadJSON, "unexpected EOF"), "invalid JSON body: unexpected EOF"; got != want {
		t.Fatalf("英文插值 = %q, want %q", got, want)
	}

	// 缺文案返回键名，界面上一眼能看出漏配。
	if got, want := msg(en, msgKey("no_such_key")), "no_such_key"; got != want {
		t.Fatalf("缺文案 = %q, want %q", got, want)
	}
}

// 文案表两版都必须有值：漏翻会直接显示中文，只有在测试里才拦得住。
func TestMessagesComplete(t *testing.T) {
	t.Parallel()

	for key, m := range messages {
		if m.zh == "" || m.en == "" {
			t.Errorf("文案 %q 缺少语言版本: zh=%q en=%q", key, m.zh, m.en)
		}
	}
}

func TestLoginErrKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		err  error
		want msgKey
	}{
		{err: errBadCredentials, want: msgBadCredentials},
		{err: errSessionToken, want: msgSessionFailed},
		{err: errCSRFToken, want: msgCSRFFailed},
	}
	for _, tc := range cases {
		if got := loginErrKey(tc.err); got != tc.want {
			t.Fatalf("loginErrKey(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
