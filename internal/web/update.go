package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"ctm/configs"
	"ctm/internal/version"
)

// 一键自更新。
//
// 分工：Web 只负责「校验 + 发起 + 记录状态 + 展示进度」，真正的下载、SHA-256 校验、
// 切换 /opt/ctm/current、健康检查与回滚仍由 deploy/install.sh 完成。
//
// 提权方式：服务以非 root 用户运行，且 unit 打开了 NoNewPrivileges 与 ProtectSystem=full，
// 因此它的更新路径只能是写请求文件，由沙箱之外的 root systemd 单元执行：
//
//	写 ${DATA_DIR}/update-request（内容 v<x.y.z>）
//	→ root 执行器（ctm-update.service → /usr/local/lib/ctm/ctm-update）取走请求
//	→ 调用 /opt/ctm/install.sh update v<x.y.z>
//	→ 结果写回 ${DATA_DIR}/update-result.json 与 update-result.log
//
// 更新会重启服务，本进程随时可能被 systemd 杀掉，因此状态先落盘，
// 下次启动时先用执行器结果、再用「当前运行版本是否等于目标版本」对账。

const (
	// defaultUpdateRunner root 侧更新执行器（install.sh 安装）
	defaultUpdateRunner = "/usr/local/lib/ctm/ctm-update"
	// defaultUpdatePathUnit systemd path 单元，用于判断一键更新是否可用
	defaultUpdatePathUnit = "/etc/systemd/system/ctm-update.path"
	// updateTimeout 单次更新最长执行时间（下载 + 切换 + 健康检查）
	updateTimeout = 10 * time.Minute
	// updateOutputMaxLen 记录的执行输出上限（保留尾部）
	updateOutputMaxLen = 4000
	// updateRunningStale 超过该时长仍标记 running 视为异常，允许重新发起
	updateRunningStale = 15 * time.Minute
)

// 更新请求的受理与结果轮询间隔。
var (
	// updateAcceptTimeout 请求文件被 root 执行器取走的最长等待时间
	updateAcceptTimeout = 30 * time.Second
	// updatePollInterval 轮询结果与版本的间隔
	updatePollInterval = 2 * time.Second
)

// 更新阶段。
const (
	updatePhaseIdle      = "idle"
	updatePhaseRunning   = "running"
	updatePhaseCompleted = "completed"
	updatePhaseFailed    = "failed"
)

// updateState 更新状态（持久化到数据目录 update-state.json）。
type updateState struct {
	Phase      string `json:"phase"`
	Target     string `json:"target"`
	From       string `json:"from"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt"`
	ExitCode   int    `json:"exitCode"`
	Output     string `json:"output"`
	Error      string `json:"error"`
	Actor      string `json:"actor"`
}

// updateResult root 执行器写回的执行结果。
type updateResult struct {
	Target     string `json:"target"`
	ExitCode   int    `json:"exitCode"`
	FinishedAt int64  `json:"finishedAt"`
}

// tagPattern 允许的版本标签（与 install.sh 执行器一致：v<x>.<y>.<z>）
var tagPattern = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// normalizeUpdateTag 校验并归一化版本号，返回不带 v 的形式。
func normalizeUpdateTag(raw string) (string, bool) {
	m := tagPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", false
	}
	return m[1] + "." + m[2] + "." + m[3], true
}

// updateRunnerPath root 侧更新执行器路径（可用 CTM_UPDATE_RUNNER 覆盖）。
func updateRunnerPath() string {
	if path := strings.TrimSpace(os.Getenv("CTM_UPDATE_RUNNER")); path != "" {
		return path
	}
	return defaultUpdateRunner
}

// updatePathUnitPath systemd path 单元路径（可用 CTM_UPDATE_PATH_UNIT 覆盖）。
func updatePathUnitPath() string {
	if path := strings.TrimSpace(os.Getenv("CTM_UPDATE_PATH_UNIT")); path != "" {
		return path
	}
	return defaultUpdatePathUnit
}

// updateUnavailable 返回一键更新不可用的原因；可用时返回空串。
func updateUnavailable() string {
	if _, err := os.Stat(updatePathUnitPath()); err != nil {
		return "未安装一键更新服务（缺少 " + updatePathUnitPath() + "）：请先在 VPS 上以 root 执行一次 deploy/install.sh install 或 update"
	}
	info, err := os.Stat(updateRunnerPath())
	if err != nil {
		return "未安装更新执行器：" + updateRunnerPath()
	}
	if info.Mode()&0o111 == 0 {
		return "更新执行器不可执行：" + updateRunnerPath()
	}
	return ""
}

// loadUpdateState 读取落盘的更新状态（不存在返回 nil）。
func (s *Server) loadUpdateState() (*updateState, error) {
	raw, err := os.ReadFile(s.cfg.UpdateStateFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state updateState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// saveUpdateState 落盘更新状态（更新会重启服务，状态必须持久化）。
func (s *Server) saveUpdateState(state *updateState) error {
	if state == nil {
		return nil
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.cfg.DataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.cfg.UpdateStateFile(), raw, 0o600)
}

// loadUpdateResult 读取 root 执行器写回的结果。
func (s *Server) loadUpdateResult() *updateResult {
	raw, err := os.ReadFile(s.cfg.UpdateResultFile())
	if err != nil {
		return nil
	}
	var result updateResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil
	}
	return &result
}

// reconcileUpdateState 启动时对账上一次更新：进程可能是在更新中途被 systemd 杀掉的。
func (s *Server) reconcileUpdateState() {
	state, err := s.loadUpdateState()
	if err != nil {
		configs.Logger.Warnw("读取更新状态失败", "err", err)
		return
	}
	if state == nil {
		return
	}
	s.updateMu.Lock()
	s.updateState = state
	s.updateMu.Unlock()
	if state.Phase != updatePhaseRunning {
		return
	}
	state.FinishedAt = time.Now().UnixMilli()
	state.Output = tailFile(s.cfg.UpdateResultLog(), updateOutputMaxLen)
	if result := s.loadUpdateResult(); result != nil && result.Target == state.Target {
		state.ExitCode = result.ExitCode
		if result.ExitCode == 0 {
			state.Phase = updatePhaseCompleted
			state.Error = ""
			configs.Logger.Infow("一键更新已完成", "target", state.Target, "exitCode", result.ExitCode)
		} else {
			state.Phase = updatePhaseFailed
			state.Error = fmt.Sprintf("更新脚本退出码 %d（详见 %s）", result.ExitCode, s.cfg.UpdateResultLog())
			configs.Logger.Errorw("一键更新未成功", "target", state.Target, "exitCode", result.ExitCode)
		}
	} else if version.Current() == state.Target {
		state.Phase = updatePhaseCompleted
		state.Error = ""
		configs.Logger.Infow("一键更新已完成（服务已运行到目标版本）", "target", state.Target)
	} else {
		state.Phase = updatePhaseFailed
		state.Error = "更新过程中服务被重启，但当前运行版本仍是 v" + version.Current() +
			"；请查看 " + s.cfg.UpdateResultLog() + " 与 /var/log/ctm-update.log"
		configs.Logger.Errorw("一键更新未成功", "target", state.Target, "current", version.Current())
	}
	if err := s.saveUpdateState(state); err != nil {
		configs.Logger.Warnw("写入更新状态失败", "err", err)
	}
}

// handleUpdateStatus 返回当前更新状态。
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"success": true,
		"current": version.Current(),
		"runner":  updateRunnerPath(),
	}
	if reason := updateUnavailable(); reason != "" {
		resp["available"] = false
		resp["unavailableReason"] = reason
	} else {
		resp["available"] = true
	}
	s.updateMu.Lock()
	state := s.updateState
	s.updateMu.Unlock()
	if state == nil {
		resp["phase"] = updatePhaseIdle
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["phase"] = state.Phase
	resp["target"] = state.Target
	resp["from"] = state.From
	resp["startedAt"] = state.StartedAt
	resp["finishedAt"] = state.FinishedAt
	resp["exitCode"] = state.ExitCode
	resp["output"] = state.Output
	resp["error"] = state.Error
	resp["actor"] = state.Actor
	writeJSON(w, http.StatusOK, resp)
}

// handleUpdateStart 发起一键更新。
func (s *Server) handleUpdateStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version string `json:"version"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	target, ok := normalizeUpdateTag(body.Version)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "版本号格式非法（只接受 1.2.3 或 v1.2.3）：" + body.Version})
		return
	}
	if reason := updateUnavailable(); reason != "" {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": reason})
		return
	}
	if current := version.Current(); current == target {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "当前已是 v" + target + "，无需更新"})
		return
	}
	// 只允许更新到最新正式版本：以版本检查结果为准，避免接口被用来降级或跳到任意版本
	check, _, err := s.latestRelease(false)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "无法确认最新版本：" + err.Error()})
		return
	}
	if check.latest != target {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"error":   "只能更新到最新正式版本 v" + check.latest + "（请求的是 v" + target + "）",
			"latest":  check.latest,
		})
		return
	}
	s.updateMu.Lock()
	if state := s.updateState; state != nil && state.Phase == updatePhaseRunning &&
		time.Since(time.UnixMilli(state.StartedAt)) < updateRunningStale {
		s.updateMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "已有更新正在进行中，请等待本次更新结束"})
		return
	}
	state := &updateState{
		Phase:     updatePhaseRunning,
		Target:    target,
		From:      version.Current(),
		StartedAt: time.Now().UnixMilli(),
		Actor:     s.administrator.Username(),
	}
	s.updateState = state
	s.updateMu.Unlock()
	if err := s.saveUpdateState(state); err != nil {
		configs.Logger.Warnw("写入更新状态失败", "err", err)
	}
	configs.Logger.Warnw("管理员发起一键更新", "username", state.Actor, "remoteIP", remoteIP(r),
		"from", state.From, "target", target)
	go s.runUpdate(target)
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true, "phase": updatePhaseRunning, "target": target})
}

// requestUpdate 落盘更新请求；写入前清掉上一轮遗留的请求与结果文件。
func (s *Server) requestUpdate(target string) error {
	for _, path := range []string{s.cfg.UpdateRequestFile(), s.cfg.UpdateResultFile(), s.cfg.UpdateResultLog()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			configs.Logger.Warnw("清理更新中间文件失败", "path", path, "err", err)
		}
	}
	if err := os.MkdirAll(s.cfg.DataDir, 0o700); err != nil {
		return err
	}
	// 先写临时文件再改名：systemd 的 PathExists 在文件刚出现时就会触发执行器，
	// 原子改名保证执行器读到的是完整标签
	temporary := s.cfg.UpdateRequestFile() + ".tmp"
	if err := os.WriteFile(temporary, []byte("v"+target+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, s.cfg.UpdateRequestFile()); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

// waitUpdateAccepted 等待 root 执行器取走请求文件。
func (s *Server) waitUpdateAccepted(ctx context.Context) error {
	deadline := time.Now().Add(updateAcceptTimeout)
	for {
		if _, err := os.Stat(s.cfg.UpdateRequestFile()); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("更新请求未被系统服务受理（请检查 ctm-update.path 是否已启用并运行）")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(updatePollInterval):
		}
	}
}

// runUpdate 异步跟踪更新结果。
func (s *Server) runUpdate(target string) {
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()
	run := s.updateRun
	if run == nil {
		run = s.defaultUpdateRun
	}
	code, output, err := run(ctx, target)
	s.updateMu.Lock()
	state := s.updateState
	if state == nil || state.Target != target || state.Phase != updatePhaseRunning {
		s.updateMu.Unlock()
		return
	}
	state.ExitCode = code
	state.Output = output
	state.FinishedAt = time.Now().UnixMilli()
	if err == nil {
		state.Phase = updatePhaseCompleted
		state.Error = ""
	} else {
		state.Phase = updatePhaseFailed
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			state.Error = "更新执行超时（" + updateTimeout.String() + "）"
		} else {
			state.Error = err.Error()
		}
	}
	snapshot := *state
	s.updateMu.Unlock()
	if saveErr := s.saveUpdateState(&snapshot); saveErr != nil {
		configs.Logger.Warnw("写入更新状态失败", "err", saveErr)
	}
	if snapshot.Phase == updatePhaseCompleted {
		configs.Logger.Infow("一键更新执行完成（若已切换版本，服务会被 systemd 重启）", "target", target)
	} else {
		configs.Logger.Errorw("一键更新执行失败", "target", target, "exitCode", code, "err", snapshot.Error)
	}
}

// defaultUpdateRun 通过 root 侧 systemd 单元执行更新。
func (s *Server) defaultUpdateRun(ctx context.Context, target string) (int, string, error) {
	if err := s.requestUpdate(target); err != nil {
		return -1, "", fmt.Errorf("写入更新请求失败: %w", err)
	}
	configs.Logger.Infow("已提交一键更新请求", "target", target, "request", s.cfg.UpdateRequestFile())
	if err := s.waitUpdateAccepted(ctx); err != nil {
		return -1, "", err
	}
	deadline := time.Now().Add(updateTimeout)
	for {
		if result := s.loadUpdateResult(); result != nil && result.Target == target {
			output := tailFile(s.cfg.UpdateResultLog(), updateOutputMaxLen)
			if result.ExitCode == 0 {
				return 0, output, nil
			}
			return result.ExitCode, output, fmt.Errorf("更新脚本退出码 %d", result.ExitCode)
		}
		// 版本已切换：正常情况下本进程已被 systemd 重启，这里只做兜底判断
		if version.Current() == target {
			return 0, tailFile(s.cfg.UpdateResultLog(), updateOutputMaxLen), nil
		}
		if time.Now().After(deadline) {
			return -1, tailFile(s.cfg.UpdateResultLog(), updateOutputMaxLen),
				fmt.Errorf("等待更新结果超时（%s），请查看 %s", updateTimeout.String(), s.cfg.UpdateResultLog())
		}
		select {
		case <-ctx.Done():
			return -1, tailFile(s.cfg.UpdateResultLog(), updateOutputMaxLen), ctx.Err()
		case <-time.After(updatePollInterval):
		}
	}
}

// tailFile 读取文件尾部用于展示更新输出。
func tailFile(path string, limit int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(raw))
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return "…" + string(runes[len(runes)-limit:])
}
