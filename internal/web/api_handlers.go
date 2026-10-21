package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-module/carbon/v2"

	"ctm/configs"
	"ctm/internal/auth"
	"ctm/internal/telecom"
	"ctm/models"
)

// handleAPI 已认证的业务 API 分发。
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	// ---- 电信账号登录 ----
	case path == "/api/telecom/start" && r.Method == http.MethodPost:
		s.handleTelecomStart(w, r)
	case path == "/api/telecom/challenge" && r.Method == http.MethodPost:
		s.handleTelecomChallenge(w, r)
	case path == "/api/telecom/verify" && r.Method == http.MethodPost:
		s.handleTelecomVerify(w, r)
	case path == "/api/telecom/sms" && r.Method == http.MethodPost:
		s.handleTelecomSms(w, r)
	case path == "/api/telecom/login" && r.Method == http.MethodPost:
		s.handleTelecomLogin(w, r)
	case path == "/api/telecom/password-login" && r.Method == http.MethodPost:
		s.handleTelecomPasswordLogin(w, r)
	case path == "/api/telecom/session" && r.Method == http.MethodGet:
		s.handleTelecomSessionGet(w, r)
	case path == "/api/telecom/session" && r.Method == http.MethodPost:
		s.handleTelecomSessionSet(w, r)

	// ---- 用量 ----
	case path == "/api/usage" && r.Method == http.MethodGet:
		s.handleUsage(w, r)
	case path == "/api/usage/refresh" && r.Method == http.MethodPost:
		s.handleUsageRefresh(w, r)
	case path == "/api/usage/raw" && r.Method == http.MethodGet:
		s.handleUsageRaw(w, r)

	// ---- 系统 ----
	case path == "/api/system/version" && r.Method == http.MethodGet:
		s.handleVersion(w, r)
	case path == "/api/system/update" && r.Method == http.MethodGet:
		s.handleUpdateStatus(w, r)
	case path == "/api/system/update" && r.Method == http.MethodPost:
		s.handleUpdateStart(w, r)

	// ---- 邮件通知与流量告警 ----
	case path == "/api/notify" && r.Method == http.MethodGet:
		s.handleNotifyGet(w, r)
	case path == "/api/notify" && r.Method == http.MethodPut:
		s.handleNotifySave(w, r)
	case path == "/api/notify/test" && r.Method == http.MethodPost:
		s.handleNotifyTest(w, r)
	case path == "/api/monitor/check" && r.Method == http.MethodPost:
		s.handleMonitorCheck(w, r)
	case path == "/api/monitor/records" && r.Method == http.MethodDelete:
		s.handleMonitorRecordsClear(w, r)

	// ---- 日志 ----
	case path == "/api/logs/dates" && r.Method == http.MethodGet:
		s.handleLogDates(w, r)
	case path == "/api/logs" && r.Method == http.MethodGet:
		s.handleLogQuery(w, r)

	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// ---- 电信登录 ----

// handleTelecomStart 记录本次要登录的手机号，重置滑块与签名。
func (s *Server) handleTelecomStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	phone := strings.TrimSpace(req.Phone)
	if len(phone) != 11 {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "请输入 11 位手机号"})
		return
	}
	s.auth.SetPhone(phone)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// handleTelecomChallenge 取登录滑块图。
func (s *Server) handleTelecomChallenge(w http.ResponseWriter, r *http.Request) {
	phone, _ := s.auth.State()
	if phone == "" {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "请先填写手机号"})
		return
	}
	ch, err := s.auth.FetchChallenge(phone)
	if err != nil {
		configs.Logger.Errorw("获取滑块失败", "phone", phone, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}
	s.auth.SetChallenge(ch)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "bg": ch.BG, "slider": ch.Slider, "size": ch.Size,
	})
}

// handleTelecomVerify 提交滑动轨迹，换滑块签名。
func (s *Server) handleTelecomVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Left   float64 `json:"left"`
		ImgW   float64 `json:"imgW"`
		PieceW float64 `json:"pieceW"`
		Track  []struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
			T float64 `json:"t"`
		} `json:"track"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ImgW <= 0 || req.PieceW <= 0 {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "参数错误"})
		return
	}
	ch := s.auth.Challenge()
	if ch == nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "请先获取滑块"})
		return
	}
	phone, _ := s.auth.State()
	if phone == "" {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "请先填写手机号"})
		return
	}
	// distance 是相对比例：拼图可移动宽度 = 图宽 - 拼图宽
	den := req.ImgW - req.PieceW
	if den < 1 {
		den = req.ImgW
	}
	distance := clamp(req.Left/den, 0, 1)

	parts := make([]string, 0, len(req.Track))
	for _, p := range req.Track {
		parts = append(parts, formatTriple(p.X, p.Y, p.T))
	}
	var durationMs int64 = 320
	if n := len(req.Track); n > 0 && req.Track[n-1].T > 0 {
		durationMs = int64(req.Track[n-1].T)
	}
	sign, err := s.auth.VerifySlider(phone, ch, distance, strings.Join(parts, "，"), durationMs)
	if err != nil {
		configs.Logger.Warnw("滑块校验失败", "phone", phone, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}
	s.auth.SetVerifySign(sign)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "distance": distance})
}

// handleTelecomSms 发送登录短信验证码。
func (s *Server) handleTelecomSms(w http.ResponseWriter, r *http.Request) {
	phone, _ := s.auth.State()
	if phone == "" {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "请先填写手机号"})
		return
	}
	// 已经发过一条时强制换新滑块，避免复用同一份签名
	s.auth.ResetSms()

	var key string
	if ch := s.auth.Challenge(); ch != nil {
		key = ch.Key
	}
	smsID, err := s.auth.SendSms(phone, key, s.auth.VerifySign())
	if errors.Is(err, auth.ErrNeedSlider) {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "needSlider": true, "error": "需要滑块验证"})
		return
	}
	if err != nil {
		configs.Logger.Warnw("发送短信验证码失败", "phone", phone, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}
	s.auth.MarkSmsSent(smsID)
	configs.Logger.Infow("短信验证码已发送", "phone", phone, "smsId", smsID)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "smsId": smsID})
}

// handleTelecomLogin 短信验证码登录。
func (s *Server) handleTelecomLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "请输入短信验证码"})
		return
	}
	phone, _ := s.auth.State()
	if phone == "" {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "请先填写手机号并获取短信验证码"})
		return
	}
	res, err := s.auth.LoginBySms(phone, code, s.auth.VerifySign())
	if errors.Is(err, auth.ErrNeedSlider) {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "needSlider": true, "error": "需要滑块验证"})
		return
	}
	if err != nil {
		configs.Logger.Warnw("短信验证码登录失败", "phone", phone, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if err := s.auth.StoreToken(res, phone); err != nil {
		configs.Logger.Errorw("登录成功但保存会话失败", "err", err)
	}
	configs.Logger.Infow("短信验证码登录成功", "phone", phone, "province", res.Province, "city", res.City)
	go s.refreshSummary()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "token": res.Token, "province": res.Province, "city": res.City,
	})
}

// handleTelecomPasswordLogin 手机号 + 密码登录（保留旧版方式）。
func (s *Server) handleTelecomPasswordLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone    string `json:"phone"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	phone := strings.TrimSpace(req.Phone)
	if len(phone) != 11 {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "请输入 11 位手机号"})
		return
	}
	if req.Password == "" {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "请输入电信 App 登录密码"})
		return
	}
	res, err := s.auth.LoginByPassword(phone, req.Password)
	if errors.Is(err, auth.ErrNeedSlider) {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "needSlider": true, "error": "上游要求滑块验证，请改用短信验证码登录"})
		return
	}
	if err != nil {
		configs.Logger.Warnw("密码登录失败", "phone", phone, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if err := s.auth.StoreToken(res, phone); err != nil {
		configs.Logger.Errorw("登录成功但保存会话失败", "err", err)
	}
	configs.Logger.Infow("密码登录成功", "phone", phone, "province", res.Province, "city", res.City)
	go s.refreshSummary()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "token": res.Token, "province": res.Province, "city": res.City,
	})
}

// handleTelecomSessionGet 查询电信登录态，token 只露头尾。
func (s *Server) handleTelecomSessionGet(w http.ResponseWriter, r *http.Request) {
	phone, token := s.auth.State()
	resp := map[string]any{"success": true, "phone": phone, "hasToken": token != ""}
	if token != "" {
		resp["token"] = maskToken(token)
	}
	if session := auth.LoadSession(); session != nil {
		resp["savedAt"] = session.SavedAt
		resp["province"] = session.Province
		resp["city"] = session.City
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleTelecomSessionSet 手动保存或清除 token。
func (s *Server) handleTelecomSessionSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
		Phone string `json:"phone"`
		Clear bool   `json:"clear"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Clear {
		if err := s.auth.SetToken("", ""); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "cleared": true})
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "请填写 token"})
		return
	}
	if err := s.auth.SetToken(strings.TrimSpace(req.Phone), strings.TrimSpace(req.Token)); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// ---- 用量 ----

// handleUsage 返回流量快照，带防抖。
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if now := carbon.Now().Timestamp(); now >= s.flowLastTime+int64(configs.IntervalsTime) {
		s.flowLastTime = now
		s.refreshSummary()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    s.desensitize(configs.Summary),
	})
}

// handleUsageRefresh 强制刷新一次并返回最新快照。
func (s *Server) handleUsageRefresh(w http.ResponseWriter, r *http.Request) {
	configs.Logger.Infow("管理员手动刷新用量", "username", s.administrator.Username(), "remoteIP", remoteIP(r))
	s.flowLastTime = carbon.Now().Timestamp()
	s.refreshSummary()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    s.desensitize(configs.Summary),
	})
}

// handleUsageRaw 上游原始响应，仅 dev 模式可用。
func (s *Server) handleUsageRaw(w http.ResponseWriter, r *http.Request) {
	if !configs.Dev {
		writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "仅 dev 模式可用"})
		return
	}
	kind := r.URL.Query().Get("kind")
	if now := carbon.Now().Timestamp(); now >= s.rawVisitLastTime+int64(configs.IntervalsTime) {
		s.rawVisitLastTime = now
		if kind == "flux" {
			data, err := s.telecom.UserFluxPackage()
			if err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
				return
			}
			s.lastFluxPackage = data
		} else {
			data, err := s.telecom.QryImportantData()
			if err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": err.Error()})
				return
			}
			s.lastImportant = data
		}
	}
	if kind == "flux" {
		writeJSON(w, http.StatusOK, s.lastFluxPackage)
		return
	}
	writeJSON(w, http.StatusOK, s.lastImportant)
}

// handleFlowPublic 兼容旧版的公开流量接口（Scriptables 面板直接调用）。
func (s *Server) handleFlowPublic(w http.ResponseWriter, r *http.Request) {
	if now := carbon.Now().Timestamp(); now >= s.flowLastTime+int64(configs.IntervalsTime) {
		s.flowLastTime = now
		s.refreshSummary()
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 200, "data": s.desensitize(configs.Summary)})
}

// RefreshSummary 拉一次汇总数据并更新快照；失败时保留上一次快照。
// 定时任务与登录成功后都调用它。
func (s *Server) RefreshSummary() {
	// 失败语义由 refreshSummary 内部的业务日志覆盖，这里不重复记录
	_, _ = s.refreshSummary()
}

// refreshSummary 拉一次汇总数据并更新快照；失败时保留上一次快照。
// 返回本次采集到的快照，供流量告警监控判断阈值；未登录或上游失败时返回 error。
func (s *Server) refreshSummary() (models.Summary, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	phone, token := s.auth.State()
	if token == "" {
		configs.Logger.Warnw("尚未登录电信账号，本次用量刷新已跳过")
		return models.Summary{}, errors.New("尚未登录电信账号")
	}
	data, err := s.telecom.QryImportantData()
	if err != nil {
		configs.Logger.Errorw("获取流量数据失败", "phone", phone, "err", err)
		return models.Summary{}, err
	}
	summary, err := telecom.ToSummary(data, phone, carbon.Now())
	if err != nil {
		configs.Logger.Errorw("换算流量数据失败", "phone", phone, "err", err)
		return models.Summary{}, err
	}
	configs.Summary = summary
	configs.Logger.Infow("用量数据已更新",
		"phone", phone,
		"flowUsed", summary.Use, "flowTotal", summary.Total,
		"generalUsed", summary.GeneralUse, "generalTotal", summary.GeneralTotal,
		"voiceUsed", summary.VoiceUsage, "voiceTotal", summary.VoiceAmount,
		"balance", summary.Balance,
	)
	return summary, nil
}

// desensitize 非 dev 模式不回显手机号，dev 模式打星。
func (s *Server) desensitize(summary models.Summary) models.Summary {
	if configs.Dev {
		summary.Username = maskPhone(summary.Username)
	} else {
		summary.Username = ""
	}
	return summary
}

// maskPhone 手机号脱敏：138****8000
func maskPhone(phone string) string {
	if len(phone) != 11 {
		return phone
	}
	return phone[:3] + "****" + phone[7:]
}

// maskToken 只露头尾。
func maskToken(token string) string {
	if len(token) <= 12 {
		return "****"
	}
	return token[:6] + "…" + token[len(token)-4:]
}

// clamp 限制取值区间。
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// formatTriple 轨迹点格式：x#y#t（与 App 上报的精度一致）
func formatTriple(x, y, t float64) string {
	return fmt.Sprintf("%.3f#%.3f#%.0f", x, y, t)
}
