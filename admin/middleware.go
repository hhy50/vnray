package admin

// middleware.go 定义管理台 HTTP 上下文与认证中间件。
import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// ctxKey 是上下文键的私有类型，避免与其他包冲突。
type ctxKey string

const (
	ctxKeySession ctxKey = "session"
	ctxKeyAuth    ctxKey = "auth"
)

// CtxSession 从请求上下文取会话；未登录返回 nil。
func CtxSession(r *http.Request) *session {
	if s, ok := r.Context().Value(ctxKeySession).(*session); ok {
		return s
	}
	return nil
}

// ctxAuth 从请求上下文取 AuthService。
func ctxAuth(r *http.Request) *AuthService {
	if a, ok := r.Context().Value(ctxKeyAuth).(*AuthService); ok {
		return a
	}
	return nil
}

// --- 响应工具 ---

// apiEnvelope 是业务接口的统一响应体：成功 code=0，失败 code 等于 HTTP 状态码。
type apiEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, apiEnvelope{Data: data})
}

func apiFail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, apiEnvelope{Code: code, Msg: msg})
}

// apiDecode 解析 JSON 请求体；失败时已经写好 400，调用方直接 return。
func apiDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		apiFailKey(w, r, http.StatusBadRequest, msgBadJSON, err)
		return false
	}
	return true
}

// actorOf 返回操作人账号，供业务日志记录"谁做的"。
func actorOf(r *http.Request) string {
	s := CtxSession(r)
	if s == nil {
		return ""
	}
	return s.Username
}

// --- 中间件 ---

// withAuth 注入 AuthService 到上下文。
func withAuth(a *AuthService, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxKeyAuth, a)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sessionMiddleware 从 cookie 加载会话，注入到上下文；不拦截未登录请求。
// 需要登录的路由再套 requireAuth。
func sessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := ctxAuth(r)
		if a == nil {
			next.ServeHTTP(w, r)
			return
		}
		token := sessionFromRequest(r)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		sess, ok := a.SessionByToken(token)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeySession, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireAuth 拦截未登录请求：JSON 请求返回 401 + JSON，页面请求 302 到登录页。
func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := CtxSession(r)
		if sess == nil {
			if wantsJSON(r) {
				apiFailKey(w, r, http.StatusUnauthorized, msgAuthRequired)
			} else {
				http.Redirect(w, r, "/admin/ui/login", http.StatusSeeOther)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrfMiddleware 校验 CSRF 令牌；仅对非 GET/HEAD/OPTIONS 的写操作生效。
func csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		sess := CtxSession(r)
		if sess == nil {
			next.ServeHTTP(w, r)
			return
		}
		got := csrfFromRequest(r)
		if !csrfValid(got, sess.CSRFToken) {
			if wantsJSON(r) {
				apiFailKey(w, r, http.StatusForbidden, msgCSRFInvalid)
			} else {
				http.Error(w, msg(r, msgCSRFInvalid), http.StatusForbidden)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// wantsJSON 判断请求是否期望 JSON 响应：Accept 头声明了 application/json，
// 或（未声明 Accept 时）路径是 /admin/ops 下的 API 段。
func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return strings.Contains(r.URL.Path, "/api/")
	}
	return strings.Contains(accept, "application/json") || strings.Contains(accept, "text/event-stream")
}
