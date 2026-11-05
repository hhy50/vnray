package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// freePort 取一个当下空闲的回环端口，供测试监听用。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode response from %s: %v (%s)", url, err, body)
	}
	return res.StatusCode, out
}

func postSQL(t *testing.T, url, stmt string) (int, map[string]any) {
	t.Helper()
	res, err := http.Post(url, "application/json", strings.NewReader(`{"sql":`+strconv.Quote(stmt)+`}`))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode response from %s: %v (%s)", url, err, body)
	}
	return res.StatusCode, out
}

func TestSQLiteWebLifecycle(t *testing.T) {
	st := seedTestStore(t)
	p := newSQLiteWebPlugin(st)
	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	ctx := context.Background()

	if err := p.Enable(ctx, map[string]string{"port": strconv.Itoa(port)}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !p.Status().Enabled {
		t.Fatal("plugin should be running after Enable")
	}

	// 表列表：store 建好的表都应可见，vn_plugin 至少存在。
	code, data := getJSON(t, base+"/api/tables")
	if code != http.StatusOK {
		t.Fatalf("GET /api/tables: status %d, body %v", code, data)
	}
	tables, _ := data["tables"].([]any)
	found := false
	for _, it := range tables {
		if m, ok := it.(map[string]any); ok && m["name"] == "vn_plugin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("vn_plugin not in tables: %v", tables)
	}

	// 执行查询语句：返回列与行。
	code, data = postSQL(t, base+"/api/exec", "SELECT COUNT(*) AS n FROM vn_plugin")
	if code != http.StatusOK {
		t.Fatalf("exec select: status %d, body %v", code, data)
	}
	if cols, _ := data["columns"].([]any); len(cols) == 0 || cols[0] != "n" {
		t.Fatalf("exec select columns = %v, want [n]", data["columns"])
	}

	// 执行写语句：返回影响行数。注意 Enable 自身会往 vn_plugin 写一行持久化记录，
	// 所以行数按插入前后的增量断言，不假设绝对值。
	_, before := getJSON(t, base+"/api/table?name=vn_plugin&limit=1&offset=0")
	baseTotal, _ := before["total"].(float64)
	code, data = postSQL(t, base+"/api/exec", "INSERT INTO vn_plugin (name, status) VALUES ('probe', 'disabled')")
	if code != http.StatusOK {
		t.Fatalf("exec insert: status %d, body %v", code, data)
	}
	if n, _ := data["rows_affected"].(float64); n != 1 {
		t.Fatalf("rows_affected = %v, want 1", data["rows_affected"])
	}

	// 浏览表数据：分页端点能读回刚插入的行。
	code, data = getJSON(t, base+"/api/table?name=vn_plugin&limit=50&offset=0")
	if code != http.StatusOK {
		t.Fatalf("GET /api/table: status %d, body %v", code, data)
	}
	if total, _ := data["total"].(float64); total != baseTotal+1 {
		t.Fatalf("total = %v, want %v", total, baseTotal+1)
	}

	// 非法表名：应被拒绝而不是 500。
	if code, _ = getJSON(t, base+"/api/table?name=x%22%20LIMIT%201--"); code == http.StatusInternalServerError {
		t.Fatal("malformed table name should not cause 500")
	}

	// 热切换端口。
	newPort := freePort(t)
	if err := p.SetPort(ctx, newPort); err != nil {
		t.Fatalf("set port: %v", err)
	}
	if _, data = getJSON(t, fmt.Sprintf("http://127.0.0.1:%d/api/tables", newPort)); data == nil {
		t.Fatal("new port should serve /api/tables")
	}

	// 停用后端口不再受理。
	if err := p.Disable(ctx); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if p.Status().Enabled {
		t.Fatal("plugin should be stopped after Disable")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := http.Get(base + "/api/tables"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listener still accepting after Disable")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 停用后 Status 继续报告上次用的端口：管理台的启用对话框靠它回显。
	if ports := p.Status().Ports; len(ports) != 1 || ports[0].Port != newPort {
		t.Fatalf("status ports after disable = %v, want %d", ports, newPort)
	}
}

// TestSQLiteWebDefaultPort 验证未配置过端口时报告内置默认端口，种子启用参数也用它。
func TestSQLiteWebDefaultPort(t *testing.T) {
	st := seedTestStore(t)
	p := newSQLiteWebPlugin(st)
	ports := p.Status().Ports
	if len(ports) != 1 || ports[0].Port != DefaultSQLiteWebPort {
		t.Fatalf("status ports = %v, want default %d", ports, DefaultSQLiteWebPort)
	}
	params := defaultEnableParams(p)
	if params["port"] != strconv.Itoa(DefaultSQLiteWebPort) {
		t.Fatalf("default enable params = %v, want port %d", params, DefaultSQLiteWebPort)
	}
}

// TestSQLiteWebEmptyRows 回归：空表与空查询结果的 rows 必须是 [] 而不是 null，
// 否则前端 rows.map 直接抛 "Cannot read properties of null"。
func TestSQLiteWebEmptyRows(t *testing.T) {
	st := seedTestStore(t)
	p := newSQLiteWebPlugin(st)
	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := p.Enable(context.Background(), map[string]string{"port": strconv.Itoa(port)}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	t.Cleanup(p.Stop)

	if code, data := postSQL(t, base+"/api/exec", "CREATE TABLE empty_probe (a INTEGER, b TEXT)"); code != http.StatusOK {
		t.Fatalf("create table: status %d, body %v", code, data)
	}

	code, data := getJSON(t, base+"/api/table?name=empty_probe&limit=50&offset=0")
	if code != http.StatusOK {
		t.Fatalf("GET /api/table: status %d, body %v", code, data)
	}
	if rows, ok := data["rows"].([]any); !ok || len(rows) != 0 {
		t.Fatalf("empty table rows = %v (%T), want []", data["rows"], data["rows"])
	}

	code, data = postSQL(t, base+"/api/exec", "SELECT * FROM empty_probe")
	if code != http.StatusOK {
		t.Fatalf("exec select empty: status %d, body %v", code, data)
	}
	if rows, ok := data["rows"].([]any); !ok || len(rows) != 0 {
		t.Fatalf("empty select rows = %v (%T), want []", data["rows"], data["rows"])
	}
}

// TestSQLiteWebBasicAuth 回归：设口令后未认证请求 401、正确口令 200、错误口令 401；
// 不带 password 参数重新启用沿用原口令；"-" 清除后恢复放行；重启后口令仍在。
func TestSQLiteWebBasicAuth(t *testing.T) {
	st := seedTestStore(t)
	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	ctx := context.Background()

	client := &http.Client{}
	get := func(user, pass string) int {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/tables", nil)
		if user != "" || pass != "" {
			req.SetBasicAuth(user, pass)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}

	p := newSQLiteWebPlugin(st)
	if err := p.Enable(ctx, map[string]string{"port": strconv.Itoa(port), "password": "s3cret"}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if code := get("", ""); code != http.StatusUnauthorized {
		t.Fatalf("no auth: status %d, want 401", code)
	}
	if code := get("admin", "s3cret"); code != http.StatusOK {
		t.Fatalf("correct password: status %d, want 200", code)
	}
	if code := get("admin", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: status %d, want 401", code)
	}

	// 不带 password 参数重新启用：口令沿用。
	if err := p.Enable(ctx, map[string]string{"port": strconv.Itoa(port)}); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if code := get("", ""); code != http.StatusUnauthorized {
		t.Fatalf("after re-enable without password: status %d, want 401", code)
	}

	// "-" 清除口令后恢复放行。
	if err := p.Enable(ctx, map[string]string{"port": strconv.Itoa(port), "password": "-"}); err != nil {
		t.Fatalf("clear password: %v", err)
	}
	if code := get("", ""); code != http.StatusOK {
		t.Fatalf("after clear: status %d, want 200", code)
	}
	p.Stop()

	// 重启后库里清除过的口令保持为空。
	restored := newSQLiteWebPlugin(st)
	restored.Restore(ctx)
	if !restored.Status().Enabled {
		t.Fatal("plugin should be running after Restore")
	}
	if code := get("", ""); code != http.StatusOK {
		t.Fatalf("restored without password: status %d, want 200", code)
	}
	restored.Stop()

	// 换口令重启：新口令生效。
	if err := p.Enable(ctx, map[string]string{"port": strconv.Itoa(port), "password": "new-pass"}); err != nil {
		t.Fatalf("enable with new password: %v", err)
	}
	p.Stop()
	restarted := newSQLiteWebPlugin(st)
	restarted.Restore(ctx)
	defer restarted.Stop()
	if code := get("admin", "new-pass"); code != http.StatusOK {
		t.Fatalf("restored with new password: status %d, want 200", code)
	}
}

func TestSQLiteWebRestore(t *testing.T) {
	st := seedTestStore(t)
	port := freePort(t)
	p := newSQLiteWebPlugin(st)
	if err := p.Enable(context.Background(), map[string]string{"port": strconv.Itoa(port)}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	p.Stop()

	// 新实例经 Restore 恢复已启用的状态并重新监听同一端口。
	restored := newSQLiteWebPlugin(st)
	restored.Restore(context.Background())
	if !restored.Status().Enabled {
		t.Fatal("plugin should be running after Restore")
	}
	_, data := getJSON(t, fmt.Sprintf("http://127.0.0.1:%d/api/tables", port))
	if data == nil {
		t.Fatal("restored plugin should serve requests")
	}
	restored.Stop()
}
