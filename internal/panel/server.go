package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vex/qqbot/internal/bot"
	"github.com/vex/qqbot/internal/onebot"
	"github.com/vex/qqbot/internal/store"
)

// sessionTTL 是登录会话有效期。
const sessionTTL = 12 * time.Hour

// 登录限流参数。
const (
	loginWindow    = time.Minute
	loginMaxPerWin = 5
	loginMaxFail   = 10
	loginLockTime  = 15 * time.Minute
)

// Server 是面板的 HTTP 服务。
type Server struct {
	store  *store.Store
	engine *bot.Engine
	client *onebot.Client
	log    *slog.Logger
	hub    *LogHub
	assets fs.FS

	startedAt time.Time

	mu       sync.Mutex
	sessions map[string]*sessionInfo
	attempts map[string]*loginAttempt

	srv *http.Server
}

type sessionInfo struct {
	user    string
	expires time.Time
}

type loginAttempt struct {
	windowStart time.Time
	count       int
	failures    int
	lockedUntil time.Time
}

// Options 是 Server 的构造参数。
type Options struct {
	Store   *store.Store
	Engine  *bot.Engine
	Client  *onebot.Client
	Log     *slog.Logger
	Hub     *LogHub
	Assets  fs.FS
	Listen  string
	Version string
}

// New 创建面板服务。
func New(opt Options) *Server {
	hub := opt.Hub
	if hub == nil {
		hub = NewLogHub(500)
	}
	return &Server{
		store:     opt.Store,
		engine:    opt.Engine,
		client:    opt.Client,
		log:       opt.Log.With("component", "panel"),
		hub:       hub,
		assets:    opt.Assets,
		startedAt: time.Now(),
		sessions:  make(map[string]*sessionInfo),
		attempts:  make(map[string]*loginAttempt),
	}
}

// Handler 返回配置好的 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 探活：不需要鉴权，供 docker healthcheck 与反代使用
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// 鉴权
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.wrap(s.handleLogout, false))
	mux.HandleFunc("GET /api/auth/status", s.wrap(s.handleAuthStatus, false))
	mux.HandleFunc("POST /api/auth/password", s.wrap(s.handleChangePassword, true))
	mux.HandleFunc("POST /api/auth/totp/setup", s.wrap(s.handleTOTPSetup, true))
	mux.HandleFunc("POST /api/auth/totp/enable", s.wrap(s.handleTOTPEnable, true))
	mux.HandleFunc("POST /api/auth/totp/disable", s.wrap(s.handleTOTPDisable, true))

	// 业务接口
	mux.HandleFunc("GET /api/overview", s.wrap(s.handleOverview, false))
	mux.HandleFunc("GET /api/settings", s.wrap(s.handleGetSettings, false))
	mux.HandleFunc("PUT /api/settings", s.wrap(s.handlePutSettings, true))
	mux.HandleFunc("GET /api/personas", s.wrap(s.handleListPersonas, false))
	mux.HandleFunc("PUT /api/personas", s.wrap(s.handleUpsertPersona, true))
	mux.HandleFunc("POST /api/personas/activate", s.wrap(s.handleActivatePersona, true))
	mux.HandleFunc("GET /api/sessions", s.wrap(s.handleListSessions, false))
	mux.HandleFunc("GET /api/sessions/messages", s.wrap(s.handleSessionMessages, false))
	mux.HandleFunc("POST /api/sessions/clear", s.wrap(s.handleClearSession, true))
	mux.HandleFunc("GET /api/usage", s.wrap(s.handleUsage, false))
	mux.HandleFunc("GET /api/audit", s.wrap(s.handleAudit, false))
	mux.HandleFunc("POST /api/test/send", s.wrap(s.handleTestSend, true))
	mux.HandleFunc("GET /api/logs", s.wrap(s.handleLogs, false))
	mux.HandleFunc("GET /api/groups", s.wrap(s.handleGroups, false))
	mux.HandleFunc("GET /api/friends", s.wrap(s.handleFriends, false))

	// 前端静态资源 + SPA 兜底
	mux.Handle("GET /", s.staticHandler())

	return s.securityHeaders(mux)
}

// Start 启动 HTTP 服务，阻塞直到 ctx 取消。
func (s *Server) Start(ctx context.Context, listen string) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // SSE 需要长连接，不能设写超时
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	s.log.Info("面板已启动", "listen", listen)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// securityHeaders 加一组保守的安全响应头。
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self'; connect-src 'self'; frame-ancestors 'self'")
		next.ServeHTTP(w, r)
	})
}

// staticHandler 提供内嵌前端，未命中的路径回落到 index.html（SPA 路由）。
func (s *Server) staticHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		f, err := s.assets.Open(p)
		if err != nil {
			// SPA 兜底
			idx, err2 := s.assets.Open("index.html")
			if err2 != nil {
				http.NotFound(w, r)
				return
			}
			defer idx.Close()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = copyAll(w, idx)
			return
		}
		defer f.Close()

		// 带 hash 的静态资源可以长缓存；index.html 必须不缓存
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if ct := contentTypeByExt(p); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		_, _ = copyAll(w, f)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"uptime": time.Since(s.startedAt).Round(time.Second).String(),
	})
}

// ---- 中间件 ----

type authedHandler func(http.ResponseWriter, *http.Request, *sessionInfo)

// wrap 包一层鉴权与 CSRF 校验。
// needCSRF 为 true 时要求请求带 X-CSRF-Token 且与 cookie 一致（双提交）。
func (s *Server) wrap(h authedHandler, needCSRF bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.currentSession(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		if needCSRF && !s.checkCSRF(r) {
			writeErr(w, http.StatusForbidden, "CSRF 校验失败，请刷新页面重试")
			return
		}
		h(w, r, sess)
	}
}

func (s *Server) currentSession(r *http.Request) (*sessionInfo, bool) {
	c, err := r.Cookie(cookieSession)
	if err != nil || c.Value == "" {
		return nil, false
	}
	key := sha256Hex(c.Value)

	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(sess.expires) {
		delete(s.sessions, key)
		return nil, false
	}
	return sess, true
}

func (s *Server) checkCSRF(r *http.Request) bool {
	// 登录接口不校验（还没有会话）
	if strings.HasSuffix(r.URL.Path, "/auth/login") {
		return true
	}
	c, err := r.Cookie(cookieCSRF)
	if err != nil || c.Value == "" {
		return false
	}
	h := r.Header.Get("X-CSRF-Token")
	if h == "" {
		return false
	}
	return subtleEqual(c.Value, h)
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, user string) error {
	token, err := randomToken(32)
	if err != nil {
		return err
	}
	csrf, err := randomToken(24)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.sessions[sha256Hex(token)] = &sessionInfo{user: user, expires: time.Now().Add(sessionTTL)}
	s.mu.Unlock()

	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     cookieSession,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
	http.SetCookie(w, &http.Cookie{
		Name:     cookieCSRF,
		Value:    csrf,
		Path:     "/",
		HttpOnly: false, // 前端需要读出来放进请求头
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
	return nil
}

func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieSession); err == nil {
		key := sha256Hex(c.Value)
		s.mu.Lock()
		delete(s.sessions, key)
		s.mu.Unlock()
	}
	for _, name := range []string{cookieSession, cookieCSRF} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
	}
}

// ---- 工具 ----

const (
	cookieSession = "qqbot_session"
	cookieCSRF    = "qqbot_csrf"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func copyAll(w http.ResponseWriter, f fs.File) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := f.Read(buf)
		if n > 0 {
			m, werr := w.Write(buf[:n])
			total += int64(m)
			if werr != nil {
				return total, werr
			}
		}
		if err != nil {
			if err.Error() == "EOF" {
				return total, nil
			}
			return total, err
		}
	}
}

func contentTypeByExt(p string) string {
	switch {
	case strings.HasSuffix(p, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(p, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(p, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(p, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(p, ".png"):
		return "image/png"
	case strings.HasSuffix(p, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(p, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(p, ".woff2"):
		return "font/woff2"
	}
	return ""
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// contextWithTimeout 从请求上下文派生一个带超时的子上下文。
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
