package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ctm/internal/version"
)

// 在线更新检查：只查询并给出更新方式，不自动替换二进制。
//
// 服务以非 root 的专用用户运行，无法切换 /opt/ctm/current 或重启 systemd，
// 因此这里返回「是否有新版本 + 一键更新入口是否可用」，实际更新由 deploy/install.sh 完成。

const (
	// defaultGithubRepo 发布仓库；可用 CTM_GITHUB_REPO 覆盖（测试或 fork）
	defaultGithubRepo = "MADAO-NW/ChinaTelecomAssistant"
	// versionCheckTTL 检查结果缓存时间，避免频繁调用 GitHub API
	versionCheckTTL = 20 * time.Minute
	// versionHTTPTimeout 单次请求超时
	versionHTTPTimeout = 10 * time.Second
	// versionNotesMaxLen Release notes 截断长度
	versionNotesMaxLen = 4000
)

// releaseInfo GitHub Release 中需要的字段。
type releaseInfo struct {
	Tag         string `json:"tag_name"`
	PublishedAt string `json:"published_at"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
}

// versionCheck 一次成功检查的结果（含抓取时间，用于缓存）。
type versionCheck struct {
	latest      string
	publishedAt string
	notes       string
	releaseURL  string
	at          int64
}

// githubRepo 发布仓库（可用环境变量覆盖）。
func githubRepo() string {
	if repo := strings.TrimSpace(os.Getenv("CTM_GITHUB_REPO")); repo != "" {
		return repo
	}
	return defaultGithubRepo
}

// githubAPIBase GitHub API 根地址（可用 CTM_GITHUB_API_BASE 指向镜像）。
func githubAPIBase() string {
	if base := strings.TrimSpace(os.Getenv("CTM_GITHUB_API_BASE")); base != "" {
		return strings.TrimSuffix(base, "/")
	}
	return "https://api.github.com"
}

// githubToken 读取只读令牌：优先环境变量（systemd EnvironmentFile 注入），其次令牌文件。
// 返回令牌与来源说明，来源只用于错误提示，不泄露令牌内容。
func githubToken() (string, string) {
	for _, key := range []string{"CTM_GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token, "环境变量 " + key
		}
	}
	path := strings.TrimSpace(os.Getenv("CTM_GITHUB_TOKEN_FILE"))
	if path == "" {
		path = "/etc/ctm/github-token"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "未配置（可读的 " + path + " 不存在）"
	}
	return strings.TrimSpace(string(raw)), "令牌文件 " + path
}

// handleVersion 返回当前版本与最新版本；query force=1 绕过缓存。
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"success":   true,
		"current":   version.Current(),
		"buildType": version.BuildType,
	}
	if reason := updateUnavailable(); reason != "" {
		resp["updaterAvailable"] = false
		resp["updaterUnavailableReason"] = reason
	} else {
		resp["updaterAvailable"] = true
		resp["updateRunner"] = updateRunnerPath()
	}
	check, cached, err := s.latestRelease(r.URL.Query().Get("force") == "1")
	if err != nil {
		resp["success"] = false
		resp["error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["latest"] = check.latest
	resp["publishedAt"] = check.publishedAt
	resp["notes"] = check.notes
	resp["releaseUrl"] = check.releaseURL
	resp["cached"] = cached
	resp["checkedAt"] = check.at
	resp["repository"] = githubRepo()
	if !version.IsRelease() {
		// 本地/开发构建不参与比较，避免误报可更新
		resp["compareSkipped"] = true
		resp["hasUpdate"] = false
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["hasUpdate"] = compareVersions(version.Current(), check.latest) < 0
	writeJSON(w, http.StatusOK, resp)
}

// latestRelease 取最新 Release（带缓存）。
func (s *Server) latestRelease(force bool) (*versionCheck, bool, error) {
	s.versionMu.Lock()
	cached := s.versionCache
	s.versionMu.Unlock()
	if !force && cached != nil && time.Since(time.UnixMilli(cached.at)) < versionCheckTTL {
		return cached, true, nil
	}
	fetch := s.releaseFetch
	if fetch == nil {
		fetch = fetchLatestRelease
	}
	info, err := fetch(githubRepo())
	if err != nil {
		return nil, false, err
	}
	check := &versionCheck{
		latest:      strings.TrimPrefix(strings.TrimSpace(info.Tag), "v"),
		publishedAt: githubDate(info.PublishedAt),
		notes:       truncateText(strings.TrimSpace(info.Body), versionNotesMaxLen),
		releaseURL:  strings.TrimSpace(info.HTMLURL),
		at:          time.Now().UnixMilli(),
	}
	s.versionMu.Lock()
	s.versionCache = check
	s.versionMu.Unlock()
	return check, false, nil
}

// fetchLatestRelease 调用 GitHub Releases API 取最新正式版本。
func fetchLatestRelease(repo string) (*releaseInfo, error) {
	token, source := githubToken()
	req, err := http.NewRequest(http.MethodGet, githubAPIBase()+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ChinaTelecomAssistant-update-check")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: versionHTTPTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("访问 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// 仓库公开：404 只可能是版本标签不存在，或还没发过 Release
		if token == "" {
			return nil, fmt.Errorf("还没有发布版本（%s 下没有 Release）", repo)
		}
		return nil, fmt.Errorf("仓库或 Release 不存在（令牌来源：%s）：%s", source, repo)
	case http.StatusUnauthorized, http.StatusForbidden:
		// 带令牌却被拒：令牌失效或权限不足；不带令牌被拒：出入口 IP 被限流
		if token == "" {
			return nil, fmt.Errorf("GitHub 拒绝匿名访问（HTTP %d），可能是 API 限流，请稍后重试或配置只读令牌", resp.StatusCode)
		}
		return nil, fmt.Errorf("GitHub 拒绝访问（HTTP %d），请检查只读令牌（来源：%s）", resp.StatusCode, source)
	case http.StatusTooManyRequests:
		return nil, errors.New("GitHub API 触发限频，请稍后再试（可在 /etc/ctm/github-token 配置只读令牌提高限额）")
	default:
		return nil, fmt.Errorf("GitHub 返回 HTTP %d", resp.StatusCode)
	}
	var info releaseInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return nil, fmt.Errorf("解析 GitHub 响应失败: %w", err)
	}
	if strings.TrimSpace(info.Tag) == "" {
		return nil, errors.New("GitHub 未返回版本标签")
	}
	return &info, nil
}

// compareVersions 比较两个版本号：a<b 返回 -1，相等 0，a>b 返回 1。
func compareVersions(a, b string) int {
	av, bv := versionParts(a), versionParts(b)
	if av == nil || bv == nil {
		return strings.Compare(a, b)
	}
	for i := 0; i < 3; i++ {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// versionParts 把 v1.2.3 解析成 3 段数字；无法解析返回 nil。
func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ".")
	out := make([]int, 3)
	for i := 0; i < len(parts) && i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return nil
		}
		out[i] = n
	}
	return out
}

// githubDate 把 RFC3339 转成北京时间日期。
func githubDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return ""
	}
	return t.In(time.FixedZone("CST", 8*60*60)).Format("2006-01-02")
}

// truncateText 按 rune 截断，避免切断多字节字符。
func truncateText(text string, limit int) string {
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
