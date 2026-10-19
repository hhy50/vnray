package admin

import (
	"net/url"
	"strings"
	"testing"
)

// TestPageNavPerLinks 覆盖分页条右下角的每页条数选项：切换后回到第 1 页，
// 且原有筛选条件必须跟着走（否则一改条数搜索条件就丢了）。
func TestPageNavPerLinks(t *testing.T) {
	raw := url.Values{"q": {"alice"}, "protocol": {"vn-v1"}, "page": {"3"}}
	nav := buildPageNav(raw, "/admin/ops/_users", 3, 20, 200)
	if len(nav.PerLinks) != len(perPageChoices) {
		t.Fatalf("per links = %d, want %d", len(nav.PerLinks), len(perPageChoices))
	}
	for i, l := range nav.PerLinks {
		if l.N != perPageChoices[i] {
			t.Fatalf("per link %d = %d, want %d", i, l.N, perPageChoices[i])
		}
		if l.Active != (l.N == 20) {
			t.Fatalf("per link %d active = %v, want %v", l.N, l.Active, l.N == 20)
		}
		if strings.Contains(l.URL, "page=") {
			t.Fatalf("per link %d must drop the page param: %s", l.N, l.URL)
		}
		for _, want := range []string{"q=alice", "protocol=vn-v1", "per="} {
			if !strings.Contains(l.URL, want) {
				t.Fatalf("per link %d lost %q: %s", l.N, want, l.URL)
			}
		}
	}
}
