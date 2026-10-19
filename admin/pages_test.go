package admin

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestBuildPageNav(t *testing.T) {
	raw := url.Values{"q": {"abc"}, "protocol": {"vmess"}}
	for _, tt := range []struct {
		name        string
		page, per   int
		total       int
		wantPage    int
		wantPages   int
		wantLinks   int
		wantHasDots bool
		wantPrev    bool
		wantNext    bool
	}{
		{name: "single page", page: 1, per: 10, total: 3, wantPage: 1, wantPages: 1, wantLinks: 1},
		{name: "middle of seven", page: 3, per: 10, total: 70, wantPage: 3, wantPages: 7, wantLinks: 7, wantPrev: true, wantNext: true},
		{name: "windowed, dots", page: 5, per: 10, total: 200, wantPage: 5, wantPages: 20, wantLinks: 7, wantHasDots: true, wantPrev: true, wantNext: true},
		{name: "page clamped to last", page: 99, per: 10, total: 25, wantPage: 3, wantPages: 3, wantLinks: 3, wantPrev: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			nav := buildPageNav(raw, "/admin/ops/_users", tt.page, tt.per, tt.total)
			if nav.Page != tt.wantPage || nav.Pages != tt.wantPages || len(nav.Links) != tt.wantLinks {
				t.Fatalf("nav = %+v, want page=%d pages=%d links=%d", nav, tt.wantPage, tt.wantPages, tt.wantLinks)
			}
			hasDots := false
			for _, l := range nav.Links {
				hasDots = hasDots || l.Dots
				if !l.Dots && !strings.Contains(l.URL, "q=abc") || !l.Dots && !strings.Contains(l.URL, "protocol=vmess") {
					t.Fatalf("link %d lost filters: %s", l.N, l.URL)
				}
				if !l.Dots && !strings.Contains(l.URL, "page="+strconv.Itoa(l.N)) {
					t.Fatalf("link %d has wrong page param: %s", l.N, l.URL)
				}
			}
			if hasDots != tt.wantHasDots {
				t.Fatalf("dots = %v, want %v", hasDots, tt.wantHasDots)
			}
			if (nav.Prev != "") != tt.wantPrev || (nav.Next != "") != tt.wantNext {
				t.Fatalf("prev/next = %q/%q, want %v/%v", nav.Prev, nav.Next, tt.wantPrev, tt.wantNext)
			}
		})
	}
}

func TestSlicePage(t *testing.T) {
	for _, tt := range []struct {
		page, per, total, wantStart, wantEnd int
	}{
		{1, 10, 25, 0, 10},
		{3, 10, 25, 20, 25},
		{9, 10, 25, 25, 25},
	} {
		start, end := slicePage(tt.page, tt.per, tt.total)
		if start != tt.wantStart || end != tt.wantEnd {
			t.Fatalf("slicePage(%d,%d,%d) = %d,%d; want %d,%d",
				tt.page, tt.per, tt.total, start, end, tt.wantStart, tt.wantEnd)
		}
	}
}
