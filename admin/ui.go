package admin

// ui.go 把 Vue 管理台的构建产物挂到 /admin/ui/。产物由 web/admin 包用
// go:embed 注册进来（缺产物时 go build 会直接失败，不会产出没有界面的二进制）；
// 没有注册（单元测试）时这些路由不生效。
//
// 壳子是公开静态：只有 JS/CSS，没有任何数据；数据接口在 /admin/ops/api/ 下，
// 一律要登录。

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// uiPath 是 SPA 的挂载点（带尾斜杠）。
const uiPath = "/admin/ui/"

// uiFS 是注册进来的前端产物；nil 表示这个二进制没有内嵌前端。
var uiFS fs.FS

// RegisterUI 由前端资源包在 init 中调用，把构建产物交给管理台；nil 表示清空
// （测试用，让"没有前端"的分支可验证）。
func RegisterUI(fsys fs.FS) {
	uiFS = fsys
}

// mountUI 把 SPA 挂到 admin 的 mux 上。
func mountUI(mux *http.ServeMux) {
	if uiFS == nil {
		return
	}
	mux.Handle("GET "+uiPath, spaHandler(uiFS))
}

// spaHandler 提供静态文件并做 history 模式回退：请求的路径不是磁盘上的文件时
// 返回 index.html，交给前端路由处理。
func spaHandler(fsys fs.FS) http.Handler {
	files := http.StripPrefix(uiPath, http.FileServer(http.FS(fsys)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.Trim(strings.TrimPrefix(r.URL.Path, uiPath), "/")
		if rel != "" && rel != "index.html" {
			if st, err := fs.Stat(fsys, path.Clean(rel)); err == nil && !st.IsDir() {
				// 构建产物带内容哈希，可以长缓存；其余（index.html 除外）交给下面。
				if strings.HasPrefix(rel, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		serveUIIndex(w, r, fsys)
	})
}

// serveUIIndex 输出 index.html。产物由 web/admin 的 embed 指令在编译期保证，
// 这里的失败只可能是注册进来的 FS 不完整——直接 500，不要伪装成页面。
func serveUIIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		http.Error(w, "embedded index.html is missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
