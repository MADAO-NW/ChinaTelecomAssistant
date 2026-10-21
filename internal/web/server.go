// Package web 管理端 Web 服务：管理员登录、用量面板、安全设置与系统更新。
//
// 页面与 API 同源：/api/auth/* 免认证，其余 /api/* 需要管理员 Session，写操作还要 CSRF 校验。
package web

import (
	"context"
	"embed"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"ctm/configs"
	"ctm/internal/admin"
	"ctm/internal/auth"
	"ctm/internal/notify"
	"ctm/internal/telecom"
	"ctm/models"
)

//go:embed static/index.html
var staticFS embed.FS

const (
	// administratorSessionCookieName 正式与临时 Session 共用的 Cookie 名
	administratorSessionCookieName = "ctm_admin_session"
	// administratorLoginCSRFCookieName 密码登录前使用的 CSRF Cookie 名
	administratorLoginCSRFCookieName = "ctm_admin_login_csrf"
	// totpIssuer 写入认证器 provisioning URI 的产品标识
	totpIssuer = "China Telecom Monitor"
	// totpQRCodeSize 安全设置页二维码边长
	totpQRCodeSize = 180
)

// Server 管理端服务。
type Server struct {
	cfg           *configs.Config
	administrator *admin.Manager
	sessions      *admin.SessionStore
	loginLimiter  *admin.LoginLimiter

	// pendingTOTP 已完成密码校验、等待输入动态码的 Session → 待确认 TOTP 密钥
	pendingTOTP   map[string]string
	pendingTOTPMu sync.Mutex
	sleep         func(time.Duration)

	// auth 电信登录态管理器；telecom 业务网关客户端
	auth    *auth.Manager
	telecom *telecom.Client

	// 采集快照与防抖
	refreshMu        sync.Mutex
	flowLastTime     int64
	rawVisitLastTime int64
	lastImportant    *models.Result[models.ImportantData]
	lastFluxPackage  *models.Result[models.UserFluxPackageData]

	// notify 邮件通知配置与流量告警存储；monitor 为后台采集器
	notifyStore *notify.Store
	monitor     *notify.Monitor

	// 版本检查与一键更新状态
	versionMu    sync.Mutex
	versionCache *versionCheck
	releaseFetch func(repo string) (*releaseInfo, error)
	updateMu     sync.Mutex
	updateState  *updateState
	updateRun    func(ctx context.Context, target string) (int, string, error)

	httpSrv *http.Server
}

// New 创建管理端服务；流量告警监控器按数据目录里的 notify.json 起一个后台采集协程。
func New(cfg *configs.Config, administrator *admin.Manager, sessions *admin.SessionStore,
	authManager *auth.Manager, telecomClient *telecom.Client) *Server {
	store := notify.NewStore(cfg.NotifyFile())
	srv := &Server{
		cfg:           cfg,
		administrator: administrator,
		sessions:      sessions,
		loginLimiter:  admin.NewLoginLimiter(),
		pendingTOTP:   map[string]string{},
		sleep:         time.Sleep,
		auth:          authManager,
		telecom:       telecomClient,
		notifyStore:   store,
	}
	srv.monitor = notify.NewMonitor(store, srv.collectSummary, nil, zapLogger{})
	return srv
}

// collectSummary 供流量告警监控采集一次用量；未登录时返回错误，不覆盖页面快照。
func (s *Server) collectSummary() (models.Summary, error) {
	return s.refreshSummary()
}

// zapLogger 把 zap 适配成 notify.Logger，避免 notify 包依赖 zap。
type zapLogger struct{}

func (zapLogger) Infof(template string, args ...any)  { configs.Logger.Infof(template, args...) }
func (zapLogger) Warnf(template string, args ...any)  { configs.Logger.Warnf(template, args...) }
func (zapLogger) Errorf(template string, args ...any) { configs.Logger.Errorf(template, args...) }

// Handler 构建 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/", s.handleAuthAPI)
	mux.Handle("/api/", s.requireAdministrator(http.HandlerFunc(s.handleAPI)))
	mux.HandleFunc("/show/flow", s.handleFlowPublic)
	mux.HandleFunc("/", s.serveIndex)
	return s.securityHeaders(mux)
}

// Start 监听并阻塞运行。
func (s *Server) Start(addr string) error {
	s.httpSrv = &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	configs.Logger.Infow("管理端已启动", "addr", addr, "dataDir", s.cfg.DataDir)
	// 对账上一次一键更新：进程可能是在更新中途被 systemd 重启的
	s.reconcileUpdateState()
	// 启动流量告警采集协程：周期与阈值都从 notify.json 读，改配置无需重启
	s.monitor.Start()
	return s.httpSrv.ListenAndServe()
}

// Shutdown 优雅关闭。
func (s *Server) Shutdown() {
	s.monitor.Shutdown()
	if s.httpSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(ctx)
	}
}

// serveIndex 返回内嵌页面。
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "页面缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// securityHeaders 统一安全响应头。
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// writeJSON 统一 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// decodeJSON 严格解析请求体，拒绝未知字段。
func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求参数格式不正确"})
		return false
	}
	return true
}

// remoteIP 取客户端 IP，用于登录限流键与审计日志。
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginRateKey 登录限流键：IP + 账号 + 阶段。
func loginRateKey(r *http.Request, account, stage string) string {
	return remoteIP(r) + "\x00" + account + "\x00" + stage
}

// errorText 错误信息兜底。
func errorText(err error) string {
	if err == nil {
		return "Session 状态已失效"
	}
	return err.Error()
}

// safeHTTPMethod 判断是否为只读方法（无需 CSRF）。
func safeHTTPMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}
