package admin

// panel.go 装配 steward 管理台：数据面、会话签名密钥、种子账号、
// 侧边栏菜单与仪表盘。选型与集成方式见 docs/adr/0028-embedded-admin-panel-steward.md
// （supersedes 0015-embedded-admin-panel.md）。

import (
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/imfiqhan/steward"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/logging"
	"github.com/hhy50/vnray/internal/transport"
)

//go:embed templates
var templatesEmbed embed.FS

// templateOverlay 是注入 steward 渲染层的页面模板：路径镜像内嵌模板树
// （pages/*.html），同名文件以它为准。
var templateOverlay = mustSub(templatesEmbed, "templates")

func mustSub(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("admin: embed templates: " + err.Error())
	}
	return sub
}

// panel 是 steward 管理台的构件集：Panel 句柄、数据面、指标缓冲与采样停止函数。
type panel struct {
	app         *steward.Panel
	db          *gorm.DB
	sampler     *sampler
	stopSampler func()
}

// panelHost 是自定义管理页的资源宿主：不承载业务数据，也不建表，只为把页面
// 挂进 steward 的路由与中间件链（登录、CSRF、操作审计）。denyPolicy 在触库前
// 就拒绝掉该资源的全部内建动作（grid/form/detail/action 均先过 ViewAny），
// 所以没有表也不会出错——**改动这个策略前必须先给它一张表**。
type panelHost struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
}

func (panelHost) TableName() string { return "panel_host_pages" }

// denyPolicy 是 panelHost 的全拒策略：ViewAny 为 false 即从侧边栏消失。
type denyPolicy struct{}

func (denyPolicy) ViewAny(*steward.Context) bool            { return false }
func (denyPolicy) View(*steward.Context, *panelHost) bool   { return false }
func (denyPolicy) Create(*steward.Context) bool             { return false }
func (denyPolicy) Update(*steward.Context, *panelHost) bool { return false }
func (denyPolicy) Delete(*steward.Context, *panelHost) bool { return false }

// newPanel 打开管理台数据面、装配 steward Panel、种子账号与菜单并启动采样。
// 页面路由在 registerPages 里挂到 s 上，须在 Build 之前完成。
//
// 数据源与业务绑定表共用：opts.DB 是业务 SQL 存储的 GORM 连接（sqlite/mysql），
// 管理台表（steward_ 前缀）与 user_bindings 同库；为 nil 属于装配失败，直接报错。
func newPanel(opts AdminOptions, s *AdminService) (*panel, error) {
	if opts.DB == nil {
		return nil, fmt.Errorf("admin: panel requires the business gorm db")
	}
	p := &panel{db: opts.DB}
	secret, err := sessionSecret()
	if err != nil {
		return nil, err
	}

	app, err := steward.New(steward.Config{
		DB:        p.db,
		SecretKey: secret,
		Prefix:    "/admin",
		Brand:     "vnray 管理台",
		// 登录与权限模型按 username 查询（非邮箱），账号种子直接用配置用户名。
		TemplatesFS:     templateOverlay,
		EnableTokenAuth: true,
		Logger:          newStewardLogger(),
	})
	if err != nil {
		return nil, fmt.Errorf("admin: create steward panel: %w", err)
	}

	p.sampler = newSampler()
	registerPages(app, s)
	registerDashboard(app, p.db, p.sampler, s.clients)

	// Build 显式调用：迁移、模板解析、菜单同步的错误在启动期暴露，
	// 而不是等到第一个请求。
	if err := app.Build(); err != nil {
		return nil, fmt.Errorf("admin: build steward panel: %w", err)
	}
	if err := seedAdminUser(p.db, opts); err != nil {
		return nil, err
	}
	if err := seedMenu(p.db); err != nil {
		return nil, err
	}
	stop := startSampler(p.sampler, s.clients)
	p.stopSampler = stop
	p.app = app
	return p, nil
}

// close 释放采样协程与 Panel 后台工作；Drain 收尾时调用，幂等。
// 库连接归业务存储所有，面板绝不替业务方关连接。
func (p *panel) close() {
	p.stopSampler()
	_ = p.app.Close()
}

// sessionSecret 生成会话签名密钥：32 字节随机值，只活在进程内存里。
// steward 用它加解密会话与 CSRF 令牌，所以重启后旧 cookie 全部失效、需要重新
// 登录——这是不落库换来的代价，换来的是少一张表。
func sessionSecret() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("admin: generate session secret: %w", err)
	}
	return key, nil
}

// seedAdminUser 保证存在可登录的管理员：配置给了口令就用配置的，否则用默认口令
// （config.DefaultAdminPassword，admin/admin）。每次启动都做 upsert——库里口令与目标
// 不一致时直接更新 bcrypt 哈希，因此忘了口令改配置重启即可。
func seedAdminUser(db *gorm.DB, opts AdminOptions) error {
	name := opts.Username
	if name == "" {
		name = config.DefaultAdminUsername
	}
	password := opts.Password
	if password == "" {
		password = config.DefaultAdminPassword
	}

	var u steward.User
	err := db.Where("username = ?", name).First(&u).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("admin: hash password: %w", err)
		}
		nu := steward.User{Username: name, Password: string(hash), Name: "Administrator"}
		if err := db.Create(&nu).Error; err != nil {
			return fmt.Errorf("admin: seed admin user: %w", err)
		}
		logging.Info("admin: admin user seeded", "username", name)
		u = nu
	case err != nil:
		return fmt.Errorf("admin: query admin user: %w", err)
	case bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(password)) != nil:
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("admin: hash password: %w", err)
		}
		if err := db.Model(&u).Update("password", string(hash)).Error; err != nil {
			return fmt.Errorf("admin: update admin password: %w", err)
		}
		logging.Info("admin: admin password updated from config", "username", name)
	}
	return grantAdministrator(db, u.ID)
}

// grantAdministrator 保证账号持有内建 Administrator 角色（该角色短路所有权限检查）。
func grantAdministrator(db *gorm.DB, userID uint) error {
	if userID == 0 {
		return nil
	}
	var role steward.Role
	if err := db.Where("slug = ?", steward.RoleAdministrator).First(&role).Error; err != nil {
		return fmt.Errorf("admin: load administrator role: %w", err)
	}
	var count int64
	db.Model(&steward.RoleUser{}).Where("role_id = ? AND user_id = ?", role.ID, userID).Count(&count)
	if count == 0 {
		if err := db.Create(&steward.RoleUser{RoleID: role.ID, UserID: userID}).Error; err != nil {
			return fmt.Errorf("admin: grant administrator role: %w", err)
		}
	}
	return nil
}

// menuEntries 是自定义页的侧边栏入口。URI 是前缀相对路径——steward 渲染时
// 会再拼 Panel 前缀（Prefix + "/" + URI），存绝对路径会变成 /admin/admin/...。
// Source=db 的行不被菜单同步触碰，按 URI 幂等补种；顺序则由代码拥有：未被管理员
// 在面板里改过的行每次启动对齐到这里的 Order，改过（Overridden）的保持原样。
// 客户端配置页是详情页，刻意不进侧边栏。
var menuEntries = []steward.MenuItem{
	{Order: 20, Title: "客户端管理", Icon: "monitor", URI: "ops/_clients", Show: true, Source: steward.MenuSourceDB},
	{Order: 30, Title: "用户管理", Icon: "users", URI: "ops/_users", Show: true, Source: steward.MenuSourceDB},
	{Order: 35, Title: "端口映射", Icon: "arrow-left-right", URI: "ops/_portmaps", Show: true, Source: steward.MenuSourceDB},
	{Order: 40, Title: "插件管理", Icon: "puzzle", URI: "ops/_plugins", Show: true, Source: steward.MenuSourceDB},
	{Order: 45, Title: "密钥管理", Icon: "key-round", URI: "ops/_keys", Show: true, Source: steward.MenuSourceDB},
	{Order: 48, Title: "服务配置", Icon: "sliders-horizontal", URI: "ops/_service-config", Show: true, Source: steward.MenuSourceDB},
	{Order: 50, Title: "日志查看", Icon: "file-text", URI: "ops/_logs", Show: true, Source: steward.MenuSourceDB},
}

// seedMenu 按 URI 幂等补种自定义页的侧边栏入口，并对齐内置页的顺序：
// 行已存在且未被管理员改过（Overridden=false）时只更新 Order，标题/图标/可见性
// 仍以库为准；管理员在面板里改过的行完全不动。
// 早期版本曾以绝对路径补种，按标题清掉这批历史错误行再补种。
func seedMenu(db *gorm.DB) error {
	for _, e := range menuEntries {
		if err := db.Where("title = ? AND uri LIKE '/%'", e.Title).Delete(&steward.MenuItem{}).Error; err != nil {
			return fmt.Errorf("admin: purge legacy menu entry %s: %w", e.Title, err)
		}
		var row steward.MenuItem
		err := db.Where("uri = ?", e.URI).First(&row).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			if err := db.Create(&e).Error; err != nil {
				return fmt.Errorf("admin: seed menu entry %s: %w", e.URI, err)
			}
		case err != nil:
			return fmt.Errorf("admin: load menu entry %s: %w", e.URI, err)
		case !row.Overridden && row.Order != e.Order:
			if err := db.Model(&row).Update("order", e.Order).Error; err != nil {
				return fmt.Errorf("admin: reorder menu entry %s: %w", e.URI, err)
			}
		}
	}
	return nil
}

// registerPages 把业务页与 JSON/动作端点挂进 steward：panelHost 资源只是载体，
// 真正的页面全部走 Resource.Page 的自定义路由，享受登录、CSRF 与操作审计。
// GET 的 _status/_bindings/_plugins 是给脚本的 JSON 快照，POST 走页面表单。
func registerPages(app *steward.Panel, s *AdminService) {
	host := steward.Register[panelHost](app).Title("管理页").Slug("ops").Icon("settings")
	host.Policy(denyPolicy{})

	host.Page("GET", "_clients", s.pageClients)
	host.Page("GET", "_client-config", s.pageClientConfig)
	host.Page("GET", "_users", s.pageUsers)
	host.Page("GET", "_plugins", s.pagePlugins)
	host.Page("GET", "_plugins-json", func(c *steward.Context) error { s.handlePlugins(c.W, c.R); return nil })
	host.Page("GET", "_logs", s.pageLogs)
	host.Page("GET", "_keys", s.pageKeys)
	host.Page("GET", "_service-config", s.pageServiceConfig)
	host.Page("GET", "_portmaps", s.pagePortMaps)
	host.Page("GET", "_status", func(c *steward.Context) error { s.handleStatus(c.W, c.R); return nil })
	host.Page("GET", "_bindings", func(c *steward.Context) error { s.handleBindingsGet(c.W, c.R); return nil })
	host.Page("POST", "_client-config", func(c *steward.Context) error { s.handleClientConfig(c.W, c.R); return nil })
	host.Page("GET", "_client-hosts", func(c *steward.Context) error { s.handleClientHosts(c.W, c.R); return nil })
	host.Page("POST", "_client-hosts", func(c *steward.Context) error { s.handleClientHosts(c.W, c.R); return nil })
	host.Page("POST", "_bindings", func(c *steward.Context) error { s.handleBindingsPost(c.W, c.R); return nil })
	host.Page("POST", "_user-config", func(c *steward.Context) error { s.handleUserConfig(c.W, c.R); return nil })
	host.Page("POST", "_client-kick", func(c *steward.Context) error { s.handleClientKick(c.W, c.R); return nil })
	host.Page("POST", "_plugin", func(c *steward.Context) error { s.handlePluginAction(c.W, c.R); return nil })
	host.Page("POST", "_auth-keys", func(c *steward.Context) error { s.handleAuthKeys(c.W, c.R); return nil })
	host.Page("POST", "_service-config", func(c *steward.Context) error { s.handleServiceConfigPost(c.W, c.R); return nil })
	host.Page("POST", "_portmaps", func(c *steward.Context) error { s.handlePortMapsPost(c.W, c.R); return nil })
}

// registerDashboard 注册仪表盘：KPI 取运行态，曲线取内存采样，饼图取操作日志。
func registerDashboard(app *steward.Panel, db *gorm.DB, smp *sampler, clients *transport.SessionManager) {
	app.Dashboard(func(d *steward.Dashboard) {
		d.Metric("在线客户端", func(*steward.Context) (any, error) { return clients.ClientCount(), nil })
		d.Metric("活跃隧道", func(*steward.Context) (any, error) {
			_, active := transport.TunnelStats()
			return active, nil
		})
		d.Metric("累计转发", func(*steward.Context) (any, error) {
			traffic, _ := transport.TunnelStats()
			return humanBytes(traffic), nil
		})
		d.Chart("在线客户端与活跃隧道", func(*steward.Context) (*steward.ChartData, error) {
			rows := smp.recent()
			clientsVals := make([]float64, 0, len(rows))
			tunnelVals := make([]float64, 0, len(rows))
			labels := make([]string, 0, len(rows))
			for _, r := range rows {
				labels = append(labels, r.At.Format("15:04:05"))
				clientsVals = append(clientsVals, float64(r.Clients))
				tunnelVals = append(tunnelVals, float64(r.Tunnels))
			}
			return &steward.ChartData{Type: steward.ChartLine, Legend: true,
				Labels: labels,
				Series: []steward.ChartSeries{
					{Label: "在线客户端", Values: clientsVals, Fill: true},
					{Label: "活跃隧道", Values: tunnelVals, Fill: true},
				}}, nil
		})
		d.Chart("转发吞吐 (B/s)", func(*steward.Context) (*steward.ChartData, error) {
			rows := smp.recent()
			chart := &steward.ChartData{Type: steward.ChartLine, Series: []steward.ChartSeries{{Label: "吞吐", Fill: true}}}
			throughput := chart.Series[0]
			for i := 1; i < len(rows); i++ {
				dt := rows[i].At.Sub(rows[i-1].At).Seconds()
				if dt <= 0 {
					continue
				}
				chart.Labels = append(chart.Labels, rows[i].At.Format("15:04:05"))
				throughput.Values = append(throughput.Values,
					float64(rows[i].Traffic-rows[i-1].Traffic)/dt)
			}
			chart.Series = []steward.ChartSeries{throughput}
			return chart, nil
		})
		d.Chart("操作分布", func(*steward.Context) (*steward.ChartData, error) {
			var rows []struct {
				Path  string
				Count int64
			}
			db.Model(&steward.OperationLog{}).Select("path, count(*) as count").Group("path").Scan(&rows)
			chart := &steward.ChartData{Type: steward.ChartPie, Legend: true}
			series := steward.ChartSeries{Label: "次数"}
			for _, r := range rows {
				chart.Labels = append(chart.Labels, r.Path)
				series.Values = append(series.Values, float64(r.Count))
			}
			chart.Series = []steward.ChartSeries{series}
			return chart, nil
		})
	})
}

// adminLogHandler 把 steward 的 slog 输出接进内部日志：访问日志降为 debug，
// 警告与错误按原级别保留。
type adminLogHandler struct{}

func (adminLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (adminLogHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make([]any, 0, r.NumAttrs()*2)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a.Key, a.Value.Any())
		return true
	})
	switch {
	case r.Level >= slog.LevelError:
		logging.Error("admin: steward: "+r.Message, attrs...)
	case r.Level >= slog.LevelWarn:
		logging.Warn("admin: steward: "+r.Message, attrs...)
	default:
		logging.Debug("admin: steward: "+r.Message, attrs...)
	}
	return nil
}

func (h adminLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h adminLogHandler) WithGroup(string) slog.Handler      { return h }

func newStewardLogger() *slog.Logger {
	return slog.New(adminLogHandler{})
}
