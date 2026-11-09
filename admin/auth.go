package admin

// auth.go 是管理台的认证与会话：单一管理员的凭证在启动时从配置播种进内存，
// 会话管理（cookie + 内存会话），CSRF 令牌。不建任何表，进程重启即回到配置态。

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/hhy50/vnray/internal/config"
	"github.com/hhy50/vnray/internal/routine"
)

// --- 会话管理 ---

// session 是一条登录会话。
type session struct {
	Username  string
	Name      string
	CSRFToken string
	ExpiresAt time.Time
}

// sessionStore 是内存会话表；进程重启全部失效。
type sessionStore struct {
	items sync.Map
}

func newSessionStore() *sessionStore {
	return &sessionStore{}
}

// sessionTTL 是会话有效期：12 小时。
const sessionTTL = 12 * time.Hour

// sessionCookieName 是会话 cookie 名。
const sessionCookieName = "vnray_admin_session"

func (s *sessionStore) get(token string) (*session, bool) {
	value, ok := s.items.Load(token)
	if !ok {
		return nil, false
	}
	sess := value.(*session)
	if time.Now().After(sess.ExpiresAt) {
		return nil, false
	}
	return sess, true
}

func (s *sessionStore) set(token string, sess *session) {
	s.items.Store(token, sess)
}

func (s *sessionStore) delete(token string) {
	s.items.Delete(token)
}

// cleanup 定期清过期会话；调用方用 routine.Go 启动。
func (s *sessionStore) cleanup(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			s.items.Range(func(key, value any) bool {
				if now.After(value.(*session).ExpiresAt) {
					s.items.CompareAndDelete(key, value)
				}
				return true
			})
		}
	}
}

// randToken 生成一个 URL 安全的随机令牌。
func randToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// --- 登录限速 ---

const (
	loginMaxFails   = 5
	loginFailWindow = 15 * time.Minute
	loginLockout    = 15 * time.Minute
)

// loginLimiter 按 IP+用户名 记录登录失败：窗口内失败达到阈值后锁定。
// 只存失败记录，量级很小；过期条目在读写时惰性清理。
type loginLimiter struct {
	fails sync.Map
}

type loginFails struct {
	count    int
	firstAt  time.Time
	lockedTo time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{}
}

// allowed 报告该 key 当前是否允许尝试登录；锁定中返回剩余等待时长。
func (l *loginLimiter) allowed(key string) (bool, time.Duration) {
	value, ok := l.fails.Load(key)
	if !ok {
		return true, 0
	}
	f := value.(*loginFails)
	now := time.Now()
	if now.After(f.lockedTo) && now.Sub(f.firstAt) > loginFailWindow {
		l.fails.CompareAndDelete(key, value)
		return true, 0
	}
	if now.Before(f.lockedTo) {
		return false, f.lockedTo.Sub(now)
	}
	return true, 0
}

// fail 记录一次失败；窗口内累计达到阈值即锁定。
func (l *loginLimiter) fail(key string) {
	now := time.Now()
	for {
		next := &loginFails{count: 1, firstAt: now}
		previous, loaded := l.fails.LoadOrStore(key, next)
		if !loaded {
			return
		}
		current := previous.(*loginFails)
		if now.Sub(current.firstAt) <= loginFailWindow {
			*next = *current
			next.count++
			if next.count >= loginMaxFails {
				next.lockedTo = now.Add(loginLockout)
			}
		}
		if l.fails.CompareAndSwap(key, previous, next) {
			return
		}
	}
}

// succeed 登录成功后清零该 key 的失败记录。
func (l *loginLimiter) succeed(key string) {
	l.fails.Delete(key)
}

// --- 认证服务 ---

// 登录失败的错误值：文案在 HTTP 边界按请求语言决定，这里只表达原因。
var (
	// errBadCredentials 同时覆盖"账号不存在"与"口令错误"，不区分以免枚举用户名。
	errBadCredentials = errors.New("invalid username or password")
	errSessionToken   = errors.New("generate session token")
	errCSRFToken      = errors.New("generate csrf token")
)

// loginErrKey 把登录失败原因映射成文案键。
func loginErrKey(err error) msgKey {
	switch {
	case errors.Is(err, errSessionToken):
		return msgSessionFailed
	case errors.Is(err, errCSRFToken):
		return msgCSRFFailed
	default:
		return msgBadCredentials
	}
}

// AuthService 封装认证与会话：凭证只存在于进程内存。
type AuthService struct {
	username string
	name     string
	passHash []byte
	sessions *sessionStore
	limiter  *loginLimiter
}

// newAuthService 构造认证服务：用户名与口令来自配置，口令在启动时 bcrypt 哈希一次。
func newAuthService(opts AdminOptions) (*AuthService, error) {
	name := opts.Username
	if name == "" {
		name = config.DefaultAdminUsername
	}
	password := opts.Password
	if password == "" {
		password = config.DefaultAdminPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("admin: hash admin password: %w", err)
	}
	return &AuthService{
		username: name,
		name:     "Administrator",
		passHash: hash,
		sessions: newSessionStore(),
		limiter:  newLoginLimiter(),
	}, nil
}

// Login 校验用户名与口令，成功返回会话令牌与 CSRF 令牌。
// 账号不存在与口令错误统一报错，且两个分支都跑一次 bcrypt，避免耗时差枚举用户名。
func (a *AuthService) Login(username, password string) (sessionToken, csrfToken string, err error) {
	if username != a.username {
		_ = bcrypt.CompareHashAndPassword(a.passHash, []byte(password))
		return "", "", errBadCredentials
	}
	if err := bcrypt.CompareHashAndPassword(a.passHash, []byte(password)); err != nil {
		return "", "", errBadCredentials
	}

	sessToken, err := randToken(32)
	if err != nil {
		return "", "", errSessionToken
	}
	csrfToken, err = randToken(32)
	if err != nil {
		return "", "", errCSRFToken
	}

	a.sessions.set(sessToken, &session{
		Username:  a.username,
		Name:      a.name,
		CSRFToken: csrfToken,
		ExpiresAt: time.Now().Add(sessionTTL),
	})

	return sessToken, csrfToken, nil
}

// Logout 注销当前会话。
func (a *AuthService) Logout(sessionToken string) {
	a.sessions.delete(sessionToken)
}

// LoginAllowed 报告该 key（IP+用户名）是否允许尝试登录；锁定中返回剩余等待时长。
func (a *AuthService) LoginAllowed(key string) (bool, time.Duration) {
	return a.limiter.allowed(key)
}

// LoginFailed 记录一次登录失败。
func (a *AuthService) LoginFailed(key string) {
	a.limiter.fail(key)
}

// LoginSucceeded 登录成功后清零失败记录。
func (a *AuthService) LoginSucceeded(key string) {
	a.limiter.succeed(key)
}

// SessionByToken 按令牌取会话；过期或不存在返回 nil。
func (a *AuthService) SessionByToken(token string) (*session, bool) {
	return a.sessions.get(token)
}

// Start 启动后台清理协程。
func (a *AuthService) Start(ctx context.Context) {
	_ = a.start(ctx)
}

func (a *AuthService) start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	routine.Go("admin.session.cleanup", func() {
		defer close(done)
		a.sessions.cleanup(ctx)
	})
	return done
}

// --- 辅助 ---

// sessionFromRequest 从 cookie 中提取会话令牌。
func sessionFromRequest(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(c.Value)
}

// csrfFromRequest 从请求头 X-CSRF-Token 中提取 CSRF 令牌。
func csrfFromRequest(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
}

// csrfValid 常量时间比较 CSRF 令牌。
func csrfValid(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
