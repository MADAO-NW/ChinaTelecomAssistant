package web

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 日志查看：读取 <data>/logs/YYYY-MM-DD.log，支持按日期、级别、关键词过滤。
// 日志由 cmd/ctm 的 dailyWriter 按日期落盘，等级标签为大写（[INFO] / [WARN] / [ERROR]）。

const (
	// logQueryDefaultMax 单次返回的最大行数（取最新的一段）。
	logQueryDefaultMax = 500
	// logQueryMaxLimit 单次请求允许的最大行数上限。
	logQueryMaxLimit = 5000
)

// logDatePattern 日志文件名中的日期格式。
var logDatePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.log$`)

// handleLogDates 返回可查询的日志日期，按倒序排列。
func (s *Server) handleLogDates(w http.ResponseWriter, r *http.Request) {
	dates := []string{}
	entries, err := os.ReadDir(s.cfg.LogDir())
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if m := logDatePattern.FindStringSubmatch(entry.Name()); m != nil {
				dates = append(dates, m[1])
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	today := time.Now().Format("2006-01-02")
	// 当天还没有日志文件时也允许选择今天，页面不会空白
	if len(dates) == 0 || dates[0] != today {
		dates = append([]string{today}, dates...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "dates": dates, "today": today})
}

// handleLogQuery 按日期、级别、关键词查询日志行；结果按时间正序返回最新的 max 行。
func (s *Server) handleLogQuery(w http.ResponseWriter, r *http.Request) {
	date := strings.TrimSpace(r.URL.Query().Get("date"))
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	if !logDatePattern.MatchString(date + ".log") {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "日期格式不正确"})
		return
	}
	level := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("level")))
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	maxLines := logQueryDefaultMax
	if raw := r.URL.Query().Get("max"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			maxLines = min(n, logQueryMaxLimit)
		}
	}

	path := filepath.Join(s.cfg.LogDir(), date+".log")
	raw, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "date": date, "level": level, "keyword": keyword,
			"lines": []string{}, "total": 0, "truncated": false,
		})
		return
	}

	// 级别过滤同时兼容三种落盘格式：
	//   console: 2026-01-01T10:00:00+0800\tINFO\t消息
	//   方括号:  [INFO] 消息
	//   json:     {"level":"INFO",...}
	levelTag := ""
	if level != "" && level != "ALL" {
		levelTag = level
	}
	matched := make([]string, 0, 256)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if levelTag != "" && !lineHasLevel(line, levelTag) {
			continue
		}
		if keyword != "" && !strings.Contains(line, keyword) {
			continue
		}
		matched = append(matched, line)
	}

	total := len(matched)
	truncated := false
	if total > maxLines {
		matched = matched[total-maxLines:]
		truncated = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"date":      date,
		"level":     level,
		"keyword":   keyword,
		"lines":     matched,
		"total":     total,
		"truncated": truncated,
	})
}

// lineHasLevel 判断日志行是否属于指定等级。
// 兼容 zap console（`\tINFO\t`）、带方括号（`[INFO]`）、JSON（`"level":"INFO"`）三种落盘格式。
func lineHasLevel(line, level string) bool {
	if strings.Contains(line, "\t"+level+"\t") || strings.Contains(line, "["+level+"]") {
		return true
	}
	// JSON 编码里等级是独立字段，加上引号与冒号避免误匹配消息正文
	return strings.Contains(line, `"level":"`+level+`"`)
}
