package sqliteweb

// sqlite-web 插件：在独立端口上提供绑定表 SQLite 库的 Web 管理页（浏览表结构、
// 分页看数据、执行任意 SQL）。复用 store 暴露的同库连接池，不另开数据库连接；
// 仅支持 sqlite 存储，默认监听 0.0.0.0——页面可设 Basic Auth 口令，建议启用。

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/hhy50/vnray/internal/plugin"
	"github.com/hhy50/vnray/internal/store"
)

// pluginName 是 sqlite-web 插件在 vn_plugin 表里的 name。
const pluginName = "sqlite-web"

// DefaultPort 是从未配置过端口时对外报告的默认监听端口：管理台的启用
// 对话框按 Status 里的端口回显，配置过之后这里报告的就是上次用的端口。
const DefaultPort = 9002

// defaultHost 是未显式配置时的监听地址：可设 Basic Auth，未设口令时
// 任意来源都能执行任意 SQL。
const defaultHost = "0.0.0.0"

//go:embed webdb.html
var sqliteWebHTML string

// WebDB 提供 Web 版 SQLite 管理界面。
type WebDB struct {
	runtime  plugin.Runtime
	run      *plugin.InboundRun
	store    store.Store
	gormDB   *gorm.DB
	sqlDB    *sql.DB
	server   *http.Server
	host     string
	port     int
	authHash string
	running  bool
}

// New 创建 SQLite 管理插件。
func New(st store.Store) *WebDB {
	var gormDB *gorm.DB
	if gs, ok := st.(store.GORMSource); ok {
		gormDB = gs.GORM()
	}
	return &WebDB{store: st, gormDB: gormDB, host: defaultHost, runtime: plugin.NewRuntime()}
}

// Name 返回插件名。
func (p *WebDB) Name() string { return pluginName }

// BindDrainGate 在注册时接入管理器的收尾状态门。
func (p *WebDB) BindDrainGate(g *plugin.DrainGate) { p.runtime.BindDrainGate(g) }

// SupportsBasicAuth 实现 BasicAuthProvider：管理台据此渲染口令输入框。
func (p *WebDB) SupportsBasicAuth() bool { return true }

// Status 返回当前状态：监听端口（未配置过时报内置默认端口）与运行中的访问地址。
func (p *WebDB) Status() plugin.Status {
	port := p.port
	if port <= 0 {
		port = DefaultPort
	}
	st := plugin.Status{
		Name:    pluginName,
		Enabled: p.running, LastError: p.runtime.LastError(),
		Ports: []plugin.PortKV{{Protocol: store.Protocol(pluginName), Port: port}},
	}
	if p.running {
		st.Params = map[string]string{"url": fmt.Sprintf("http://%s/", net.JoinHostPort(p.host, strconv.Itoa(p.port)))}
		if p.authHash == "" {
			st.Params["auth"] = "open"
		} else {
			st.Params["auth"] = "basic"
		}
	}
	return st
}

// Restore 恢复监听配置并把错误返回宿主。
func (p *WebDB) Restore(ctx context.Context) error {
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	cur, ok, err := p.store.GetPlugin(ctx, pluginName)
	if err != nil {
		return p.runtime.Record(err)
	}
	if port := cur.Config.Ports[store.Protocol(pluginName)]; port > 0 {
		p.port = port
	}
	if h := cur.Config.Host; h != "" {
		p.host = h
	}
	p.authHash = cur.Config.Password
	if !ok || cur.Status != store.PluginEnabled {
		return nil
	}
	if p.port <= 0 {
		return p.runtime.Record(fmt.Errorf("plugin: invalid sqlite-web port %d", p.port))
	}
	return p.runtime.Record(p.configure(ctx, p.port, p.host, p.authHash, false))
}

// Enable 启用插件：校验并持久化端口、监听地址与访问口令，然后立即开始服务。
// params 里 port 必填，host 可选（缺省沿用已存值或 0.0.0.0）；password 可选——
// 空串沿用已存口令，"-" 清除口令，其余值按 bcrypt 哈希后保存。
func (p *WebDB) Enable(ctx context.Context, params map[string]string) error {
	port, err := strconv.Atoi(params["port"])
	if err != nil || port <= 0 || port >= 65536 {
		return fmt.Errorf("plugin: invalid sqlite-web port %q", params["port"])
	}
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	host := strings.TrimSpace(params["host"])
	if host == "" {
		host = p.host
	}
	if err := validateListenHost(host); err != nil {
		return err
	}
	authHash := p.authHash
	switch raw := params["password"]; {
	case raw == "-":
		authHash = ""
	case raw != "":
		h, err := bcrypt.GenerateFromPassword([]byte(raw), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("plugin: sqlite-web hash password: %w", err)
		}
		authHash = string(h)
	}
	return p.runtime.Record(p.configure(ctx, port, host, authHash, true))
}

// Disable 停用插件：停止服务并持久化状态。
func (p *WebDB) Disable(ctx context.Context) error {
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	if err := p.save(ctx, store.PluginDisabled, p.port, p.host, p.authHash); err != nil {
		return err
	}
	p.stop()
	return nil
}

// SetPort 在新监听及持久化均成功后替换运行态。
func (p *WebDB) SetPort(ctx context.Context, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("plugin: invalid sqlite-web port %d", port)
	}
	if p.runtime.IsDraining() {
		return plugin.ErrDraining
	}
	if !p.running {
		if err := p.save(ctx, "", port, p.host, p.authHash); err != nil {
			return p.runtime.Record(err)
		}
		p.port = port
		return p.runtime.Record(nil)
	}
	return p.runtime.Record(p.configure(ctx, port, p.host, p.authHash, true))
}

func (p *WebDB) configure(ctx context.Context, port int, host, authHash string, persist bool) error {
	if p.gormDB == nil {
		return fmt.Errorf("plugin: sqlite-web requires a sql-backed store")
	}
	if name := p.gormDB.Dialector.Name(); name != "sqlite" {
		return fmt.Errorf("plugin: sqlite-web only supports sqlite storage, got %q", name)
	}
	db, err := p.gormDB.DB()
	if err != nil {
		return fmt.Errorf("plugin: sqlite-web acquire db: %w", err)
	}
	var ln net.Listener
	if !p.running || p.port != port || p.host != host {
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("plugin: sqlite-web listen %s: %w", addr, err)
		}
	}
	if persist {
		if err := p.save(ctx, store.PluginEnabled, port, host, authHash); err != nil {
			if ln != nil {
				_ = ln.Close()
			}
			return err
		}
	}
	if ln == nil {
		p.authHash = authHash
		return nil
	}
	p.stop()
	p.host, p.port, p.authHash, p.sqlDB, p.running = host, port, authHash, db, true
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", p.serveHome)
	mux.HandleFunc("GET /api/tables", p.serveTables)
	mux.HandleFunc("GET /api/table", p.serveTableRows)
	mux.HandleFunc("POST /api/exec", p.serveExec)
	server := &http.Server{Handler: p.withBasicAuth(mux), ReadHeaderTimeout: 10 * time.Second}
	p.server = server
	run, err := p.runtime.StartInbound("plugin.sqliteweb.serve", ln, func(ctx context.Context) plugin.Server {
		server.BaseContext = func(net.Listener) context.Context { return ctx }
		return plugin.NewHTTPInbound(server)
	}, func(err error) {
		if p.server == server {
			p.running = false
			p.runtime.Record(err)
		}
	})
	p.run = run
	if err != nil {
		p.running = false
	}
	return err
}

// Drain 停止接入并等待 HTTP 请求退出。
func (p *WebDB) Drain(ctx context.Context) error {
	runs := p.runtime.BeginDrain()
	p.running = false
	err := plugin.DrainRuns(ctx, runs)
	p.sqlDB = nil
	return err
}

// Stop 停止接入，当前请求继续收尾。
func (p *WebDB) Stop() {
	p.stop()
}

func (p *WebDB) save(ctx context.Context, status store.PluginStatus, port int, host, authHash string) error {
	return plugin.SaveState(ctx, p.store, pluginName, status, func(c *store.PluginConfig) {
		c.Ports[store.Protocol(pluginName)] = port
		// 默认监听地址不入库：留空即表示"用内置默认值"。
		if host == defaultHost {
			host = ""
		}
		c.Host = host
		c.Password = authHash
	})
}

func (p *WebDB) stop() {
	if p.run != nil {
		p.run.Stop()
	}
	if p.server != nil {
		p.server.SetKeepAlivesEnabled(false)
	}
	p.server, p.running = nil, false
}

// withBasicAuth 对页面与 API 做 Basic Auth（用户名不限，只校验口令）；口令未设置时
// 直接放行。哈希在请求时读取，运行中改口令立即生效，无需重启监听。
func (p *WebDB) withBasicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash := p.authHash
		if hash == "" {
			next.ServeHTTP(w, r)
			return
		}
		if _, pass, ok := r.BasicAuth(); ok && bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) == nil {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="sqlite-web", charset="UTF-8"`)
		httpFail(w, http.StatusUnauthorized, "unauthorized")
	})
}

// validateListenHost 校验监听地址：只接受裸主机名或 IP，拒绝带协议/路径的写法。
func validateListenHost(host string) error {
	if host == "" || strings.ContainsAny(host, "/?# ") || strings.Contains(host, "://") {
		return fmt.Errorf("plugin: sqlite-web host %q must be a bare hostname or IP", host)
	}
	return nil
}

func (p *WebDB) serveHome(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(sqliteWebHTML))
}

// serveTables 返回全部表与视图：名字、建表语句与行数。
func (p *WebDB) serveTables(w http.ResponseWriter, r *http.Request) {
	db, fail := p.db(w)
	if fail {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx,
		`SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		httpFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	type tableInfo struct {
		Name string `json:"name"`
		DDL  string `json:"ddl"`
		Rows int64  `json:"rows"`
	}
	// 连接池是单连接（store 侧 MaxOpenConns(1)），必须先读完游标释放连接，
	// 再逐表发 COUNT 查询；边遍历边查会把唯一连接占死。
	var tables []tableInfo
	for rows.Next() {
		var t tableInfo
		if err := rows.Scan(&t.Name, &t.DDL); err != nil {
			rows.Close()
			httpFail(w, http.StatusInternalServerError, err.Error())
			return
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		httpFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows.Close()
	for i := range tables {
		tables[i].Rows, _ = p.countRows(ctx, db, tables[i].Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tables": tables})
}

// serveTableRows 分页返回一张表的全部列数据。
func (p *WebDB) serveTableRows(w http.ResponseWriter, r *http.Request) {
	db, fail := p.db(w)
	if fail {
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		httpFail(w, http.StatusBadRequest, "missing table name")
		return
	}
	limit := clampInt(queryInt(r, "limit", 50), 1, 500)
	offset := max(queryInt(r, "offset", 0), 0)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	total, err := p.countRows(ctx, db, name)
	if err != nil {
		httpFail(w, http.StatusBadRequest, err.Error())
		return
	}
	data, cols, truncated, err := queryAll(ctx, db,
		fmt.Sprintf("SELECT * FROM %s LIMIT ? OFFSET ?", quoteIdent(name)), []any{limit, offset}, limit)
	if err != nil {
		httpFail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"columns": cols, "rows": data, "total": total, "truncated": truncated,
	})
}

// serveExec 执行任意 SQL：有结果集的语句返回行数据，其余返回影响行数。
func (p *WebDB) serveExec(w http.ResponseWriter, r *http.Request) {
	db, fail := p.db(w)
	if fail {
		return
	}
	var in struct {
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpFail(w, http.StatusBadRequest, "invalid request body")
		return
	}
	stmt := strings.TrimSpace(in.SQL)
	if stmt == "" {
		httpFail(w, http.StatusBadRequest, "empty sql")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	// 不能靠 Query 报错来探测语句类型：SQLite 驱动对 INSERT 等写语句的 Query
	// 返回空结果集而不报错。按前缀区分——返回结果集的语句只有这几类。
	if returnsRows(stmt) {
		rows, err := db.QueryContext(ctx, stmt)
		if err != nil {
			httpFail(w, http.StatusBadRequest, err.Error())
			return
		}
		defer rows.Close()
		data, cols, truncated, err := scanRows(rows, 1000)
		if err != nil {
			httpFail(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"columns": cols, "rows": data, "truncated": truncated,
		})
		return
	}
	res, err := db.ExecContext(ctx, stmt)
	if err != nil {
		httpFail(w, http.StatusBadRequest, err.Error())
		return
	}
	affected, _ := res.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]any{"rows_affected": affected})
}

// returnsRows 判断一条 SQL 是否返回结果集（SQLite 中只有这几类前缀）。
func returnsRows(stmt string) bool {
	s := strings.TrimSpace(stmt)
	for _, p := range []string{"select", "values", "with", "pragma", "explain"} {
		if strings.HasPrefix(strings.ToLower(s), p) {
			return true
		}
	}
	return false
}

// db 返回当前可用的数据库连接；插件已停用时响应 503。
func (p *WebDB) db(w http.ResponseWriter) (*sql.DB, bool) {
	db := p.sqlDB
	if db == nil {
		httpFail(w, http.StatusServiceUnavailable, "sqlite-web is stopping")
		return nil, true
	}
	return db, false
}

// countRows 统计一张表的行数；表名经引号转义后拼进语句。
func (p *WebDB) countRows(ctx context.Context, db *sql.DB, name string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdent(name)).Scan(&n)
	return n, err
}

// queryInt 解析整型查询参数；非法或缺失时返回默认值。
func queryInt(r *http.Request, key string, def int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	return n
}

func clampInt(v, lo, hi int) int { return min(max(v, lo), hi) }

// quoteIdent 把标识符（表名/列名）用双引号转义，防注入。
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// scanRows 读完全部行，max 之上截断；[]byte（BLOB）转字符串便于 JSON 编码。
// data 初始化为空切片而非 nil：nil 会被 JSON 编码成 null，前端得对空结果特判。
func scanRows(rows *sql.Rows, max int) (data [][]any, cols []string, truncated bool, err error) {
	data = make([][]any, 0, 16)
	cols, err = rows.Columns()
	if err != nil {
		return nil, nil, false, err
	}
	for rows.Next() {
		ptrs := make([]any, len(cols))
		for i := range ptrs {
			ptrs[i] = new(any)
		}
		if err := rows.Scan(ptrs...); err != nil {
			return data, cols, truncated, err
		}
		row := make([]any, len(cols))
		for i, ptr := range ptrs {
			v := *ptr.(*any)
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			row[i] = v
		}
		data = append(data, row)
		if len(data) >= max {
			truncated = true
			break
		}
	}
	return data, cols, truncated, rows.Err()
}

// queryAll 执行带参数的查询并读回结果。
func queryAll(ctx context.Context, db *sql.DB, query string, args []any, max int) ([][]any, []string, bool, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	return scanRows(rows, max)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpFail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// Name 是 sqlite-web 插件在 vn_plugin 表与配置里的名字。
const Name = pluginName

// InitialState 返回首次初始化写入的插件记录：默认端口与监听地址就地给出，
// 由装配层与本包以外的插件记录合并。
func InitialState() store.PluginState {
	return store.PluginState{
		Name:   pluginName,
		Status: store.PluginEnabled,
		Config: store.PluginConfig{
			Ports: map[store.Protocol]int{store.Protocol(pluginName): DefaultPort},
			Host:  defaultHost,
		},
	}
}
