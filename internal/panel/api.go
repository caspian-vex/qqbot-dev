package panel

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vex/qqbot/internal/onebot"
	"github.com/vex/qqbot/internal/store"
)

// ---- 鉴权 ----

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TOTP     string `json:"totp"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if ok, wait := s.allowLogin(ip); !ok {
		writeErr(w, http.StatusTooManyRequests,
			fmt.Sprintf("尝试过于频繁，请 %d 分钟后再试", int(wait.Minutes())+1))
		return
	}

	var req loginReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	settings, err := s.store.Settings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取配置失败")
		return
	}

	user := settings.Panel.AdminUser
	if user == "" {
		user = "admin"
	}

	okUser := subtleEqual(req.Username, user)
	okPass := settings.Panel.PasswordHash != "" && VerifyPassword(req.Password, settings.Panel.PasswordHash)
	if !okUser || !okPass {
		s.recordFailure(ip)
		s.store.RecordAudit("login.failed", "用户名或密码错误", ip)
		// 统一提示，不透露是用户名错还是密码错
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}

	if settings.Panel.TOTPEnabled {
		if !VerifyTOTP(settings.Panel.TOTPSecret, req.TOTP) {
			s.recordFailure(ip)
			s.store.RecordAudit("login.failed", "TOTP 校验失败", ip)
			writeErr(w, http.StatusUnauthorized, "动态验证码错误")
			return
		}
	}

	if err := s.issueSession(w, r, user); err != nil {
		writeErr(w, http.StatusInternalServerError, "创建会话失败")
		return
	}
	s.resetFailures(ip)
	s.store.RecordAudit("login.ok", "登录成功", ip)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"user":         user,
		"totp_enabled": settings.Panel.TOTPEnabled,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	s.clearSession(w, r)
	s.store.RecordAudit("logout", sess.user, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	settings, err := s.store.Settings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取配置失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":         sess.user,
		"totp_enabled": settings.Panel.TOTPEnabled,
		"expires_at":   sess.expires,
	})
}

type passwordReq struct {
	Old string `json:"old"`
	New string `json:"new"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	var req passwordReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(req.New) < 12 {
		writeErr(w, http.StatusBadRequest, "新密码至少 12 位")
		return
	}

	settings, err := s.store.Settings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取配置失败")
		return
	}
	if settings.Panel.PasswordHash != "" && !VerifyPassword(req.Old, settings.Panel.PasswordHash) {
		writeErr(w, http.StatusUnauthorized, "原密码错误")
		return
	}

	hash, err := HashPassword(req.New)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成密码哈希失败")
		return
	}
	if _, err := s.store.UpdateSettings(func(v *store.Settings) error {
		v.Panel.PasswordHash = hash
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	s.store.RecordAudit("password.changed", sess.user, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	secret, err := NewTOTPSecret()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成密钥失败")
		return
	}
	settings, _ := s.store.Settings()
	issuer := "qqbot"
	url := TOTPProvisioningURL(issuer, settings.Panel.AdminUser, secret)

	// 先存下来但不开用，等用户提交一次正确验证码再启用
	if _, err := s.store.UpdateSettings(func(v *store.Settings) error {
		v.Panel.TOTPSecret = secret
		v.Panel.TOTPEnabled = false
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"secret": secret,
		"url":    url,
	})
}

type totpCodeReq struct {
	Code string `json:"code"`
}

func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	var req totpCodeReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	settings, err := s.store.Settings()
	if err != nil || settings.Panel.TOTPSecret == "" {
		writeErr(w, http.StatusBadRequest, "请先生成 TOTP 密钥")
		return
	}
	if !VerifyTOTP(settings.Panel.TOTPSecret, req.Code) {
		writeErr(w, http.StatusBadRequest, "验证码不正确")
		return
	}
	if _, err := s.store.UpdateSettings(func(v *store.Settings) error {
		v.Panel.TOTPEnabled = true
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	s.store.RecordAudit("totp.enabled", sess.user, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	var req totpCodeReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	settings, err := s.store.Settings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取配置失败")
		return
	}
	// 关闭二步验证需要当前验证码，防止会话被盗后直接关掉
	if settings.Panel.TOTPEnabled && !VerifyTOTP(settings.Panel.TOTPSecret, req.Code) {
		writeErr(w, http.StatusBadRequest, "验证码不正确")
		return
	}
	if _, err := s.store.UpdateSettings(func(v *store.Settings) error {
		v.Panel.TOTPEnabled = false
		v.Panel.TOTPSecret = ""
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	s.store.RecordAudit("totp.disabled", sess.user, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 总览 ----

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	st := s.client.Status()
	stats := s.engine.Stats()
	settings, _ := s.store.Settings()

	day := time.Now().Format("2006-01-02")
	usage, _ := s.store.UsageForDay(day)

	online := false
	if st.Connected {
		if ok, err := s.client.GetOnline(r.Context()); err == nil {
			online = ok
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"uptime":        time.Since(s.startedAt).Round(time.Second).String(),
		"self_id":       st.SelfID,
		"nickname":      st.Nickname,
		"connected":     st.Connected,
		"online":        online,
		"connected_at":  st.ConnectedAt,
		"last_event_at": st.LastEventAt,
		"reconnects":    st.Reconnects,
		"last_error":    st.LastError,
		"stats":         stats,
		"usage_today":   usage,
		"model":         settings.LLM.Model,
		"totp_enabled":  settings.Panel.TOTPEnabled,
		"dedupe_size":   s.engine.DedupeSize(),
	})
}

// ---- 设置 ----

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	v, err := s.store.Settings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取配置失败")
		return
	}
	out := *v
	// 永远不回传明文 Key，只告诉前端"有没有配"
	out.LLM.APIKey = maskKey(v.LLM.APIKey)
	out.Panel.PasswordHash = ""
	out.Panel.TOTPSecret = ""
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	cur, err := s.store.Settings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取配置失败")
		return
	}

	// 在当前配置的副本上解码，而不是从零值开始。
	// 这样只传一部分字段也能正确更新；从零值解码会让缺省字段变空，
	// 校验直接报"模型地址不能为空"——调用方传的明明是合法请求。
	in := *cur
	in.Panel.PasswordHash = ""
	in.Panel.TOTPSecret = ""
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := validateSettings(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	updated, err := s.store.UpdateSettings(func(v *store.Settings) error {
		v.LLM = in.LLM
		// 前端传掩码或空串表示"不修改"
		if in.LLM.APIKey == "" || strings.Contains(in.LLM.APIKey, "****") {
			v.LLM.APIKey = cur.LLM.APIKey
		}
		v.Bot = in.Bot
		v.Context = in.Context
		// 面板凭据只能通过专门的接口改，这里不覆盖
		v.Panel = cur.Panel
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	s.store.RecordAudit("settings.updated", sess.user, clientIP(r))

	out := *updated
	out.LLM.APIKey = maskKey(updated.LLM.APIKey)
	out.Panel.PasswordHash = ""
	out.Panel.TOTPSecret = ""
	writeJSON(w, http.StatusOK, out)
}

func validateSettings(v *store.Settings) error {
	if v.LLM.BaseURL == "" {
		return fmt.Errorf("模型地址不能为空")
	}
	if !strings.HasPrefix(v.LLM.BaseURL, "http") {
		return fmt.Errorf("模型地址必须以 http(s):// 开头")
	}
	if v.LLM.Model == "" {
		return fmt.Errorf("模型名称不能为空")
	}
	if v.LLM.MaxTokens <= 0 || v.LLM.MaxTokens > 8192 {
		return fmt.Errorf("最大输出 token 应在 1-8192 之间")
	}
	if v.LLM.TimeoutSec <= 0 || v.LLM.TimeoutSec > 300 {
		return fmt.Errorf("超时时间应在 1-300 秒之间")
	}
	if v.Bot.PassiveRate < 0 || v.Bot.PassiveRate > 1 {
		return fmt.Errorf("被动接话概率应在 0-1 之间")
	}
	if v.Bot.MaxConcurrent < 1 || v.Bot.MaxConcurrent > 10 {
		return fmt.Errorf("并发数应在 1-10 之间")
	}
	if v.Bot.MaxSegments < 1 || v.Bot.MaxSegments > 8 {
		return fmt.Errorf("最大分段数应在 1-8 之间")
	}
	if v.Context.MaxTurns < 0 || v.Context.MaxTurns > 50 {
		return fmt.Errorf("上下文轮数应在 0-50 之间")
	}
	if v.Context.TokenBudget < 200 || v.Context.TokenBudget > 100000 {
		return fmt.Errorf("上下文 token 预算应在 200-100000 之间")
	}
	return nil
}

// ---- 人格 ----

func (s *Server) handleListPersonas(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	list, err := s.store.ListPersonas()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取人格失败")
		return
	}
	active, _ := s.store.ActivePersona()
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  nonNil(list),
		"active": active,
	})
}

type personaReq struct {
	Name         string `json:"name"`
	SystemPrompt string `json:"system_prompt"`
}

func (s *Server) handleUpsertPersona(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	var req personaReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "人格名不能为空")
		return
	}
	if len(req.SystemPrompt) > 32*1024 {
		writeErr(w, http.StatusBadRequest, "提示词过长（上限 32KB）")
		return
	}
	p, err := s.store.UpsertPersona(req.Name, req.SystemPrompt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	s.store.RecordAudit("persona.saved", req.Name, clientIP(r))
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleActivatePersona(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := s.store.ActivatePersona(req.ID); err != nil {
		writeErr(w, http.StatusBadRequest, "切换失败")
		return
	}
	s.store.RecordAudit("persona.activated", strconv.FormatInt(req.ID, 10), clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 会话 ----

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	list, err := s.store.ListSessions()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取会话失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(list)})
}

// handleSessionMessages 返回某会话最近的对话记录，供面板查看上下文。
func (s *Server) handleSessionMessages(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "会话 ID 不能为空")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	msgs, err := s.store.RecentMessages(id, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取消息失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(msgs)})
}

func (s *Server) handleClearSession(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "会话 ID 不能为空")
		return
	}
	if err := s.store.ClearSession(req.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "清空失败")
		return
	}
	s.store.RecordAudit("session.cleared", req.ID, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 用量与审计 ----

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 30 {
		days = 7
	}
	list, err := s.store.RecentUsage(days)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取用量失败")
		return
	}
	today, _ := s.store.UsageForDay(time.Now().Format("2006-01-02"))
	writeJSON(w, http.StatusOK, map[string]any{
		"today": today,
		"items": nonNil(list),
	})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	list, err := s.store.ListAudit(100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取审计失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(list)})
}

// ---- 测试发送 ----

type testSendReq struct {
	TargetType string `json:"target_type"` // group | private
	TargetID   int64  `json:"target_id"`
	Text       string `json:"text"`
}

func (s *Server) handleTestSend(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	var req testSendReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeErr(w, http.StatusBadRequest, "消息内容不能为空")
		return
	}
	if req.TargetID == 0 {
		writeErr(w, http.StatusBadRequest, "目标不能为空")
		return
	}

	var (
		id  int64
		err error
	)
	switch req.TargetType {
	case "group":
		id, err = s.client.SendGroupMsg(r.Context(), req.TargetID, onebot.TextSegments(req.Text))
	case "private":
		id, err = s.client.SendPrivateMsg(r.Context(), req.TargetID, onebot.TextSegments(req.Text))
	default:
		writeErr(w, http.StatusBadRequest, "目标类型只能是 group 或 private")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "发送失败："+err.Error())
		return
	}
	s.store.RecordAudit("test.send", fmt.Sprintf("%s:%d", req.TargetType, req.TargetID), clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message_id": id})
}

// ---- 群与好友 ----

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	list, err := s.client.GetGroupList(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "拉取群列表失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(list)})
}

func (s *Server) handleFriends(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	list, err := s.client.GetFriendList(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "拉取好友列表失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(list)})
}

// ---- 日志 SSE ----

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request, sess *sessionInfo) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "当前服务器不支持流式响应")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 让反代不要缓冲

	enc := json.NewEncoder(w)
	send := func(l LogLine) bool {
		if _, err := fmt.Fprint(w, "data: "); err != nil {
			return false
		}
		if err := enc.Encode(l); err != nil {
			return false
		}
		if _, err := fmt.Fprint(w, "\n"); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// 先补发最近的日志
	for _, l := range s.hub.Recent(100) {
		if !send(l) {
			return
		}
	}

	ch, cancel := s.hub.Subscribe()
	defer cancel()

	// 心跳，防止反代掐断空闲连接
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case l, ok := <-ch:
			if !ok {
				return
			}
			if !send(l) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---- 辅助 ----

func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "****" + k[len(k)-4:]
}

func clientIP(r *http.Request) string {
	// 经过 Cloudflare 与 OpenResty 两层反代，优先取 CF 的头
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) allowLogin(ip string) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a := s.attempts[ip]
	if a == nil {
		a = &loginAttempt{windowStart: time.Now()}
		s.attempts[ip] = a
	}
	now := time.Now()
	if now.Before(a.lockedUntil) {
		return false, a.lockedUntil.Sub(now)
	}
	if now.Sub(a.windowStart) > loginWindow {
		a.windowStart = now
		a.count = 0
	}
	if a.count >= loginMaxPerWin {
		return false, loginWindow - now.Sub(a.windowStart)
	}
	a.count++
	return true, 0
}

func (s *Server) recordFailure(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attempts[ip]
	if a == nil {
		a = &loginAttempt{windowStart: time.Now()}
		s.attempts[ip] = a
	}
	a.failures++
	if a.failures >= loginMaxFail {
		a.lockedUntil = time.Now().Add(loginLockTime)
		a.failures = 0
	}
}

func (s *Server) resetFailures(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.attempts, ip)
}

// nonNil 把 nil 切片换成空切片再序列化。
//
// 为什么必须做：Go 的 nil 切片编码成 JSON 是 null，不是 []。
// 前端拿到 null 后调 .map / .length 会直接抛异常，整个页面白屏。
// 契约上凡是叫 items 的字段，永远应该是数组。
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
