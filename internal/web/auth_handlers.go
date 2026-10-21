package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"ctm/configs"
	"ctm/internal/admin"
)

// authStatusResponse 登录状态：前端据此决定显示密码表单还是动态码表单。
type authStatusResponse struct {
	Authenticated    bool   `json:"authenticated"`
	Stage            string `json:"stage"`
	Username         string `json:"username,omitempty"`
	TwoFactorEnabled bool   `json:"twoFactorEnabled"`
	CSRFToken        string `json:"csrfToken"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type totpCodeRequest struct {
	Code string `json:"code"`
}

type passwordChangeRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
	ConfirmPassword string `json:"confirmPassword"`
}

type usernameChangeRequest struct {
	CurrentPassword string `json:"currentPassword"`
	Username        string `json:"username"`
}

type currentPasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
}

type disableTOTPRequest struct {
	CurrentPassword string `json:"currentPassword"`
	Code            string `json:"code"`
}

type totpSetupResponse struct {
	Secret          string `json:"secret"`
	ProvisioningURI string `json:"provisioningUri"`
	QRCodeDataURL   string `json:"qrCodeDataUrl"`
}

// handleAuthAPI 认证相关路由分发。
func (s *Server) handleAuthAPI(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/auth/status" && r.Method == http.MethodGet:
		s.handleAuthStatus(w, r)
	case r.URL.Path == "/api/auth/login" && r.Method == http.MethodPost:
		s.handleAdministratorLogin(w, r)
	case r.URL.Path == "/api/auth/2fa/verify" && r.Method == http.MethodPost:
		s.handleLoginTOTP(w, r)
	case r.URL.Path == "/api/auth/2fa/cancel" && r.Method == http.MethodPost:
		s.handleCancelLoginTOTP(w, r)
	case r.URL.Path == "/api/auth/logout" && r.Method == http.MethodPost:
		s.handleAdministratorLogout(w, r)
	case r.URL.Path == "/api/auth/password" && r.Method == http.MethodPost:
		s.handleChangeAdministratorPassword(w, r)
	case r.URL.Path == "/api/auth/username" && r.Method == http.MethodPost:
		s.handleChangeAdministratorUsername(w, r)
	case r.URL.Path == "/api/auth/2fa/setup" && r.Method == http.MethodPost:
		s.handleSetupTOTP(w, r)
	case r.URL.Path == "/api/auth/2fa/enable" && r.Method == http.MethodPost:
		s.handleEnableTOTP(w, r)
	case r.URL.Path == "/api/auth/2fa/disable" && r.Method == http.MethodPost:
		s.handleDisableTOTP(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// handleAuthStatus 返回当前登录阶段，未登录时下发登录 CSRF Cookie。
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	sessionID, csrfToken, authenticated, err := s.currentAdministratorSession(r)
	if err != nil {
		configs.Logger.Errorw("刷新管理员 Session 失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法刷新登录状态"})
		return
	}
	if authenticated {
		s.setAdministratorSessionCookie(w, sessionID)
		writeJSON(w, http.StatusOK, authStatusResponse{
			Authenticated:    true,
			Stage:            "authenticated",
			Username:         s.administrator.Username(),
			TwoFactorEnabled: s.administrator.TOTPEnabled(),
			CSRFToken:        csrfToken,
		})
		return
	}
	if _, csrfToken, ok := s.pendingAdministratorSession(r); ok {
		writeJSON(w, http.StatusOK, authStatusResponse{Stage: "totp", CSRFToken: csrfToken})
		return
	}
	token, err := newAuthToken()
	if err != nil {
		configs.Logger.Errorw("生成登录 CSRF token 失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法初始化登录"})
		return
	}
	s.setAuthCookie(w, administratorLoginCSRFCookieName, token)
	writeJSON(w, http.StatusOK, authStatusResponse{Stage: "password", CSRFToken: token})
}

// handleAdministratorLogin 校验用户名密码；启用 2FA 时转入待验证阶段。
func (s *Server) handleAdministratorLogin(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(administratorLoginCSRFCookieName)
	if err != nil || !sameAuthToken(cookie.Value, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "登录请求已失效，请刷新页面重试"})
		return
	}
	var request loginRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	rateKey := loginRateKey(r, request.Username, "password")
	configs.Logger.Infow("管理员密码登录开始", "username", request.Username, "remoteIP", remoteIP(r))
	if !s.administrator.Verify(request.Username, request.Password) {
		s.sleep(s.loginLimiter.Failure(rateKey))
		configs.Logger.Warnw("管理员密码登录失败", "username", request.Username, "remoteIP", remoteIP(r))
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "用户名或密码错误"})
		return
	}
	s.loginLimiter.Success(rateKey)
	if s.administrator.TOTPEnabled() {
		sessionID, csrfToken, err := s.sessions.CreatePending()
		if err != nil {
			configs.Logger.Errorw("创建双因素临时 Session 失败", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法登录"})
			return
		}
		s.setAuthCookie(w, administratorSessionCookieName, sessionID)
		s.deleteAuthCookie(w, administratorLoginCSRFCookieName)
		configs.Logger.Infow("管理员密码验证通过，等待双因素认证", "username", request.Username)
		writeJSON(w, http.StatusAccepted, authStatusResponse{Stage: "totp", CSRFToken: csrfToken})
		return
	}
	sessionID, csrfToken, err := s.sessions.Create()
	if err != nil {
		configs.Logger.Errorw("创建管理员 Session 失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法登录"})
		return
	}
	s.setAdministratorSessionCookie(w, sessionID)
	s.deleteAuthCookie(w, administratorLoginCSRFCookieName)
	configs.Logger.Infow("管理员登录成功", "username", request.Username, "remoteIP", remoteIP(r), "twoFactor", false)
	writeJSON(w, http.StatusOK, authStatusResponse{
		Authenticated: true,
		Stage:         "authenticated",
		Username:      s.administrator.Username(),
		CSRFToken:     csrfToken,
	})
}

// handleLoginTOTP 校验动态码并把临时 Session 提升为正式 Session。
func (s *Server) handleLoginTOTP(w http.ResponseWriter, r *http.Request) {
	sessionID, csrfToken, ok := s.pendingAdministratorSession(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "双因素登录已失效，请重新登录"})
		return
	}
	if !sameAuthToken(csrfToken, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "登录请求已失效，请刷新页面重试"})
		return
	}
	var request totpCodeRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	rateKey := loginRateKey(r, s.administrator.Username(), "totp")
	if !s.administrator.VerifyAndConsumeTOTP(request.Code) {
		s.sleep(s.loginLimiter.Failure(rateKey))
		configs.Logger.Warnw("管理员双因素登录失败", "username", s.administrator.Username(), "remoteIP", remoteIP(r))
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "动态验证码错误"})
		return
	}
	s.loginLimiter.Success(rateKey)
	newSessionID, newCSRFToken, promoted, err := s.sessions.PromotePending(sessionID)
	if err != nil || !promoted {
		configs.Logger.Errorw("轮换双因素登录 Session 失败", "err", errorText(err))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法登录"})
		return
	}
	s.setAdministratorSessionCookie(w, newSessionID)
	configs.Logger.Infow("管理员登录成功", "username", s.administrator.Username(), "remoteIP", remoteIP(r), "twoFactor", true)
	writeJSON(w, http.StatusOK, authStatusResponse{
		Authenticated:    true,
		Stage:            "authenticated",
		Username:         s.administrator.Username(),
		TwoFactorEnabled: true,
		CSRFToken:        newCSRFToken,
	})
}

// handleCancelLoginTOTP 取消双因素登录，回到密码阶段。
func (s *Server) handleCancelLoginTOTP(w http.ResponseWriter, r *http.Request) {
	sessionID, csrfToken, ok := s.pendingAdministratorSession(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "双因素登录已失效"})
		return
	}
	if !sameAuthToken(csrfToken, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "登录请求已失效，请刷新页面重试"})
		return
	}
	if err := s.sessions.Delete(sessionID); err != nil {
		configs.Logger.Errorw("取消双因素登录失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法取消双因素登录"})
		return
	}
	s.deleteAuthCookie(w, administratorSessionCookieName)
	s.handleAuthStatus(w, r)
}

// handleAdministratorLogout 退出登录。
func (s *Server) handleAdministratorLogout(w http.ResponseWriter, r *http.Request) {
	sessionID, _, ok := s.requireAuthenticatedAuthRequest(w, r)
	if !ok {
		return
	}
	if err := s.sessions.Delete(sessionID); err != nil {
		configs.Logger.Errorw("管理员退出登录失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法退出登录"})
		return
	}
	s.pendingTOTPMu.Lock()
	delete(s.pendingTOTP, sessionID)
	s.pendingTOTPMu.Unlock()
	s.deleteAuthCookie(w, administratorSessionCookieName)
	configs.Logger.Infow("管理员退出登录", "username", s.administrator.Username(), "remoteIP", remoteIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// handleChangeAdministratorUsername 修改用户名；成功后所有 Session 失效。
func (s *Server) handleChangeAdministratorUsername(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAuthenticatedAuthRequest(w, r)
	if !ok {
		return
	}
	var request usernameChangeRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	previousUsername := s.administrator.Username()
	rateKey := loginRateKey(r, previousUsername, "security")
	if !s.administrator.Verify(previousUsername, request.CurrentPassword) {
		s.securityFailure(w, r, rateKey, "当前密码错误")
		return
	}
	if err := s.administrator.ChangeUsername(request.CurrentPassword, request.Username); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	s.loginLimiter.Success(rateKey)
	if err := s.invalidateAdministratorSessions(w); err != nil {
		configs.Logger.Errorw("管理员用户名已修改但清理 Session 失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "用户名已修改，但清理登录状态失败，请重新登录", "reauthenticate": true})
		return
	}
	configs.Logger.Infow("管理员用户名修改成功", "previousUsername", previousUsername, "username", request.Username, "remoteIP", remoteIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "reauthenticate": true})
}

// handleChangeAdministratorPassword 修改密码；成功后所有 Session 失效。
func (s *Server) handleChangeAdministratorPassword(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAuthenticatedAuthRequest(w, r)
	if !ok {
		return
	}
	var request passwordChangeRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.NewPassword != request.ConfirmPassword {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "两次输入的新密码不一致"})
		return
	}
	rateKey := loginRateKey(r, s.administrator.Username(), "security")
	if !s.administrator.Verify(s.administrator.Username(), request.CurrentPassword) {
		s.securityFailure(w, r, rateKey, "当前密码错误")
		return
	}
	if err := s.administrator.ChangePassword(request.CurrentPassword, request.NewPassword); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	s.loginLimiter.Success(rateKey)
	if err := s.invalidateAdministratorSessions(w); err != nil {
		configs.Logger.Errorw("管理员密码已修改但清理 Session 失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "密码已修改，但清理登录状态失败，请重新登录", "reauthenticate": true})
		return
	}
	configs.Logger.Infow("管理员密码修改成功", "username", s.administrator.Username(), "remoteIP", remoteIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "reauthenticate": true})
}

// handleSetupTOTP 生成 TOTP 密钥与二维码，等待用户输入动态码确认。
func (s *Server) handleSetupTOTP(w http.ResponseWriter, r *http.Request) {
	sessionID, _, ok := s.requireAuthenticatedAuthRequest(w, r)
	if !ok {
		return
	}
	if s.administrator.TOTPEnabled() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "双因素认证已经启用"})
		return
	}
	var request currentPasswordRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	rateKey := loginRateKey(r, s.administrator.Username(), "security")
	if !s.administrator.Verify(s.administrator.Username(), request.CurrentPassword) {
		s.securityFailure(w, r, rateKey, "当前密码错误")
		return
	}
	secret, err := admin.GenerateTOTPSecret()
	if err != nil {
		configs.Logger.Errorw("生成 TOTP 密钥失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法开始双因素认证设置"})
		return
	}
	uri, err := admin.ProvisioningURI(totpIssuer, s.administrator.Username(), secret)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法生成双因素认证配置"})
		return
	}
	png, err := qrcode.Encode(uri, qrcode.Medium, totpQRCodeSize)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法生成双因素认证二维码"})
		return
	}
	s.pendingTOTPMu.Lock()
	s.pendingTOTP[sessionID] = secret
	s.pendingTOTPMu.Unlock()
	s.loginLimiter.Success(rateKey)
	writeJSON(w, http.StatusOK, totpSetupResponse{
		Secret:          secret,
		ProvisioningURI: uri,
		QRCodeDataURL:   "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
	})
}

// handleEnableTOTP 用动态码确认并启用 TOTP。
func (s *Server) handleEnableTOTP(w http.ResponseWriter, r *http.Request) {
	sessionID, _, ok := s.requireAuthenticatedAuthRequest(w, r)
	if !ok {
		return
	}
	if s.administrator.TOTPEnabled() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "双因素认证已经启用"})
		return
	}
	var request totpCodeRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	s.pendingTOTPMu.Lock()
	secret := s.pendingTOTP[sessionID]
	s.pendingTOTPMu.Unlock()
	if secret == "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "请先重新生成双因素认证配置"})
		return
	}
	step, verified := admin.VerifyTOTP(secret, request.Code, time.Now(), -1)
	if !verified {
		s.securityFailure(w, r, loginRateKey(r, s.administrator.Username(), "security"), "动态验证码错误")
		return
	}
	if err := s.administrator.EnableTOTP(secret, step); err != nil {
		configs.Logger.Errorw("启用双因素认证失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法启用双因素认证"})
		return
	}
	s.pendingTOTPMu.Lock()
	delete(s.pendingTOTP, sessionID)
	s.pendingTOTPMu.Unlock()
	if err := s.invalidateAdministratorSessions(w); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "双因素认证已启用，但清理登录状态失败，请重新登录", "reauthenticate": true})
		return
	}
	configs.Logger.Infow("管理员双因素认证启用成功", "username", s.administrator.Username(), "remoteIP", remoteIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "reauthenticate": true})
}

// handleDisableTOTP 需要当前密码 + 有效动态码才能停用。
func (s *Server) handleDisableTOTP(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAuthenticatedAuthRequest(w, r)
	if !ok {
		return
	}
	if !s.administrator.TOTPEnabled() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "双因素认证尚未启用"})
		return
	}
	var request disableTOTPRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	rateKey := loginRateKey(r, s.administrator.Username(), "security")
	if !s.administrator.Verify(s.administrator.Username(), request.CurrentPassword) {
		s.securityFailure(w, r, rateKey, "当前密码错误")
		return
	}
	if !s.administrator.VerifyAndConsumeTOTP(request.Code) {
		s.securityFailure(w, r, rateKey, "动态验证码错误")
		return
	}
	if err := s.administrator.DisableTOTP(); err != nil {
		configs.Logger.Errorw("停用双因素认证失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法停用双因素认证"})
		return
	}
	if err := s.invalidateAdministratorSessions(w); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "双因素认证已停用，但清理登录状态失败，请重新登录", "reauthenticate": true})
		return
	}
	configs.Logger.Infow("管理员双因素认证停用成功", "username", s.administrator.Username(), "remoteIP", remoteIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "reauthenticate": true})
}

// requireAdministrator 认证中间件：校验 Session，写操作再校验 CSRF。
func (s *Server) requireAdministrator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionID, csrfToken, ok, err := s.currentAdministratorSession(r)
		if err != nil {
			configs.Logger.Errorw("刷新管理员 Session 失败", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法刷新登录状态"})
			return
		}
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "管理员登录已失效"})
			return
		}
		s.setAdministratorSessionCookie(w, sessionID)
		if !safeHTTPMethod(r.Method) && !sameAuthToken(csrfToken, r.Header.Get("X-CSRF-Token")) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求已失效，请刷新页面重试"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAuthenticatedAuthRequest 认证 + CSRF 校验，返回 Session 与 CSRF token。
func (s *Server) requireAuthenticatedAuthRequest(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	sessionID, csrfToken, ok, err := s.currentAdministratorSession(r)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂时无法刷新登录状态"})
		return "", "", false
	}
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "管理员登录已失效"})
		return "", "", false
	}
	s.setAdministratorSessionCookie(w, sessionID)
	if !sameAuthToken(csrfToken, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求已失效，请刷新页面重试"})
		return "", "", false
	}
	return sessionID, csrfToken, true
}

// currentAdministratorSession 读取正式 Session。
func (s *Server) currentAdministratorSession(r *http.Request) (string, string, bool, error) {
	cookie, err := r.Cookie(administratorSessionCookieName)
	if err != nil {
		return "", "", false, nil
	}
	csrfToken, ok, err := s.sessions.Get(cookie.Value)
	return cookie.Value, csrfToken, ok, err
}

// pendingAdministratorSession 读取待双因素验证的临时 Session。
func (s *Server) pendingAdministratorSession(r *http.Request) (string, string, bool) {
	cookie, err := r.Cookie(administratorSessionCookieName)
	if err != nil {
		return "", "", false
	}
	csrfToken, ok := s.sessions.GetPending(cookie.Value)
	return cookie.Value, csrfToken, ok
}

// invalidateAdministratorSessions 让全部 Session 失效并清理待确认 TOTP。
func (s *Server) invalidateAdministratorSessions(w http.ResponseWriter) error {
	err := s.sessions.DeleteAll(s.administrator.CredentialFingerprint())
	s.pendingTOTPMu.Lock()
	clear(s.pendingTOTP)
	s.pendingTOTPMu.Unlock()
	s.deleteAuthCookie(w, administratorSessionCookieName)
	return err
}

// securityFailure 记录安全校验失败并按限流策略延迟响应。
func (s *Server) securityFailure(w http.ResponseWriter, r *http.Request, rateKey, message string) {
	s.sleep(s.loginLimiter.Failure(rateKey))
	configs.Logger.Warnw("管理员安全验证失败", "username", s.administrator.Username(), "remoteIP", remoteIP(r), "reason", message)
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": message})
}

// setAuthCookie 设置 HttpOnly + SameSite=Strict 会话 Cookie。
func (s *Server) setAuthCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// setAdministratorSessionCookie 设置带有效期的会话 Cookie。
func (s *Server) setAdministratorSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     administratorSessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(admin.AdministratorSessionLifetime / time.Second),
		Expires:  time.Now().Add(admin.AdministratorSessionLifetime),
	})
}

// deleteAuthCookie 清除 Cookie。
func (s *Server) deleteAuthCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// newAuthToken 生成 32 字节随机 token。
func newAuthToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// sameAuthToken 常量时间比较。
func sameAuthToken(expected, actual string) bool {
	if expected == "" || len(expected) != len(actual) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}
