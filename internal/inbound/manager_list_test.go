package inbound_test

import (
	"reflect"
	"testing"

	"github.com/hhy50/vnray/admin"
)

func TestManagerListOrder(t *testing.T) {
	st := testStore(t)
	m := newTestManager(st, admin.NewBindings(st), nil, nil)
	want := []string{"sqlite-web", "ss", "v2ray", "vnray-user"}
	for range 20 {
		statuses := m.List()
		got := make([]string, len(statuses))
		for i, status := range statuses {
			got[i] = status.Name
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("plugin order = %v, want %v", got, want)
		}
	}
}
