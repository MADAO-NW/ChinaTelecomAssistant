package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"ctm/configs"
	"ctm/internal/mailer"
	"ctm/internal/notify"
)

// 邮件通知配置与流量告警的 HTTP 接口。
//
// 配置统一存在数据目录的 notify.json：
//   - 「邮箱配置」页只改发信参数（SMTP 与发件人）；
//   - 「流量监控」页只改阈值、周期区间、启停开关与收件人。
//
// 两个页面各自提交时只覆盖自己负责的字段，避免互相冲掉对方的设置。

// 配置校验失败的提示，避免在多个分支里重复拼接文案。
var errInvalidThreshold = errors.New("「剩余多少 GB 时提醒」要填一个大于 0 的数字")

// notifyInput 两个页面共用的提交结构；零值字段按「未提交」处理。
type notifyInput struct {
	Enabled            *bool    `json:"enabled"`
	Threshold          *int64   `json:"threshold"`
	IntervalMinSeconds *int     `json:"intervalMinSeconds"`
	IntervalMaxSeconds *int     `json:"intervalMaxSeconds"`
	Recipients         []string `json:"recipients"`
	SmtpHost           *string  `json:"smtpHost"`
	SmtpPort           *int     `json:"smtpPort"`
	SmtpUsername       *string  `json:"smtpUsername"`
	SmtpPassword       *string  `json:"smtpPassword"`
	SmtpFrom           *string  `json:"smtpFrom"`
	SmtpEncryption     *string  `json:"smtpEncryption"`
	// TestRecipients 只用于「发送测试」这一次发送，不写回配置
	TestRecipients []string `json:"testRecipients"`
}

// handleNotifyGet 返回完整配置与运行状态；密码只回传「是否已设置」。
func (s *Server) handleNotifyGet(w http.ResponseWriter, r *http.Request) {
	settings, err := s.notifyStore.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	senderErr := ""
	if err := settings.ValidateSender(); err != nil {
		senderErr = err.Error()
	}
	recipientsErr := ""
	if err := mailer.ValidateRecipients(settings.Recipients); err != nil {
		recipientsErr = err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":            true,
		"enabled":            settings.Enabled,
		"threshold":          settings.Threshold,
		"intervalMinSeconds": settings.IntervalMinSeconds,
		"intervalMaxSeconds": settings.IntervalMaxSeconds,
		"minInterval":        notify.MinIntervalSeconds,
		"maxInterval":        notify.MaxIntervalSeconds,
		"smtpHost":           settings.Host,
		"smtpPort":           settings.Port,
		"smtpUsername":       settings.Username,
		"smtpFrom":           settings.From,
		"smtpEncryption":     settings.Encryption,
		"recipients":         settings.Recipients,
		"passwordSet":        settings.Password != "",
		"senderConfigured":   senderErr == "",
		"senderError":        senderErr,
		"recipientsValid":    recipientsErr == "",
		"recipientsError":    recipientsErr,
		"records":            settings.Records,
		"updatedAt":          settings.UpdatedAt,
		"status":             s.monitor.Status(),
	})
}

// handleNotifySave 保存发信配置与告警配置；只覆盖请求里提交的字段。
func (s *Server) handleNotifySave(w http.ResponseWriter, r *http.Request) {
	var input notifyInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "请求内容格式不正确"})
		return
	}
	previous, err := s.notifyStore.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	next := previous
	if input.Enabled != nil {
		next.Enabled = *input.Enabled
	}
	if input.Threshold != nil {
		next.Threshold = *input.Threshold
	}
	if input.IntervalMinSeconds != nil {
		next.IntervalMinSeconds = *input.IntervalMinSeconds
	}
	if input.IntervalMaxSeconds != nil {
		next.IntervalMaxSeconds = *input.IntervalMaxSeconds
	}
	if input.Recipients != nil {
		next.Recipients = normalizeRecipients(input.Recipients)
	}
	if input.SmtpHost != nil {
		next.Host = strings.TrimSpace(*input.SmtpHost)
	}
	if input.SmtpPort != nil {
		next.Port = *input.SmtpPort
	}
	if input.SmtpUsername != nil {
		next.Username = strings.TrimSpace(*input.SmtpUsername)
	}
	if input.SmtpFrom != nil {
		next.From = strings.TrimSpace(*input.SmtpFrom)
	}
	if input.SmtpEncryption != nil {
		encryption, err := mailer.NormalizeEncryption(*input.SmtpEncryption)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
			return
		}
		next.Encryption = encryption
	}
	// 密码不提交（nil）表示保持原值；显式提交空串表示清除
	if input.SmtpPassword != nil {
		next.Password = *input.SmtpPassword
	}

	if err := validateNotifyInput(input, next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}
	next.UpdatedAt = time.Now().Unix()
	if err := s.notifyStore.Save(next); err != nil {
		configs.Logger.Errorw("保存邮件与告警配置失败", "username", s.administrator.Username(), "remoteIP", remoteIP(r), "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "没保存成功: " + err.Error()})
		return
	}
	// 逐字段记录原值与新值；SMTP 密码只记录是否变化
	configs.Logger.Infow("邮件与告警配置已更新",
		"username", s.administrator.Username(), "remoteIP", remoteIP(r),
		"previousEnabled", previous.Enabled, "enabled", next.Enabled,
		"previousThreshold", previous.Threshold, "threshold", next.Threshold,
		"previousIntervalMinSeconds", previous.IntervalMinSeconds, "intervalMinSeconds", next.IntervalMinSeconds,
		"previousIntervalMaxSeconds", previous.IntervalMaxSeconds, "intervalMaxSeconds", next.IntervalMaxSeconds,
		"previousRecipients", previous.Recipients, "recipients", next.Recipients,
		"previousSmtpHost", previous.Host, "smtpHost", next.Host,
		"previousSmtpPort", previous.Port, "smtpPort", next.Port,
		"previousSmtpUsername", previous.Username, "smtpUsername", next.Username,
		"previousSmtpFrom", previous.From, "smtpFrom", next.From,
		"previousSmtpEncryption", previous.Encryption, "smtpEncryption", next.Encryption,
		"passwordChanged", previous.Password != next.Password,
	)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// validateNotifyInput 按提交的字段分组校验，避免只改告警参数时被未配置的发信参数拦住。
func validateNotifyInput(input notifyInput, next notify.Settings) error {
	senderTouched := input.SmtpHost != nil || input.SmtpPort != nil || input.SmtpUsername != nil ||
		input.SmtpPassword != nil || input.SmtpFrom != nil || input.SmtpEncryption != nil
	if senderTouched {
		if err := next.ValidateSender(); err != nil {
			return err
		}
	}
	if input.Recipients != nil {
		if err := mailer.ValidateRecipients(next.Recipients); err != nil {
			return err
		}
	}
	alertTouched := input.Enabled != nil || input.Threshold != nil ||
		input.IntervalMinSeconds != nil || input.IntervalMaxSeconds != nil
	if alertTouched {
		if next.Threshold <= 0 {
			return errInvalidThreshold
		}
		return notify.ValidateInterval(next.IntervalMinSeconds, next.IntervalMaxSeconds)
	}
	return nil
}

// normalizeRecipients 去空白、去重，保留提交顺序。
func normalizeRecipients(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		address := strings.TrimSpace(value)
		if address == "" {
			continue
		}
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		out = append(out, address)
	}
	return out
}

// handleNotifyTest 用已保存的发信配置给流量告警的全部收件人发一封测试邮件。
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	var input notifyInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "请求内容格式不正确"})
		return
	}
	settings, err := s.notifyStore.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	// 以已保存的配置为底，叠加页面上当前填写、尚未保存的值：测试就是验证「现在填的这套」能不能发出去
	sender := settings.Config
	if input.SmtpHost != nil {
		sender.Host = strings.TrimSpace(*input.SmtpHost)
	}
	if input.SmtpPort != nil {
		sender.Port = *input.SmtpPort
	}
	if input.SmtpUsername != nil {
		sender.Username = strings.TrimSpace(*input.SmtpUsername)
	}
	if input.SmtpFrom != nil {
		sender.From = strings.TrimSpace(*input.SmtpFrom)
	}
	if input.SmtpPassword != nil {
		sender.Password = *input.SmtpPassword
	}
	if input.SmtpEncryption != nil {
		encryption, err := mailer.NormalizeEncryption(*input.SmtpEncryption)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
			return
		}
		sender.Encryption = encryption
	}
	// 测试收件人只用于这一次发送，不写回配置
	recipients := normalizeRecipients(input.TestRecipients)
	sent, err := s.monitor.TestMail(sender, recipients)
	if err != nil {
		configs.Logger.Errorw("测试邮件发送失败",
			"username", s.administrator.Username(), "remoteIP", remoteIP(r),
			"smtpHost", sender.Host, "smtpPort", sender.Port, "recipients", recipients, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "error": err.Error()})
		return
	}
	configs.Logger.Infow("测试邮件已发送",
		"username", s.administrator.Username(), "remoteIP", remoteIP(r),
		"smtpHost", sender.Host, "smtpPort", sender.Port, "recipients", sent)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "recipients": sent})
}

// handleMonitorCheck 立即采集一轮并返回结果；采集顺带完成阈值判断。
func (s *Server) handleMonitorCheck(w http.ResponseWriter, r *http.Request) {
	configs.Logger.Infow("管理员手动检测流量", "username", s.administrator.Username(), "remoteIP", remoteIP(r))
	snapshot := s.monitor.CollectNow()
	writeJSON(w, http.StatusOK, map[string]any{
		"success":  snapshot.Error == "",
		"error":    snapshot.Error,
		"snapshot": snapshot,
		"status":   s.monitor.Status(),
	})
}

// handleMonitorRecordsClear 清空告警记录，保留邮件与告警配置。
func (s *Server) handleMonitorRecordsClear(w http.ResponseWriter, r *http.Request) {
	settings, err := s.notifyStore.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	previousCount := len(settings.Records)
	settings.Records = nil
	if err := s.notifyStore.Save(settings); err != nil {
		configs.Logger.Errorw("清空流量告警记录失败", "username", s.administrator.Username(), "remoteIP", remoteIP(r), "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	configs.Logger.Infow("流量告警记录已清空",
		"username", s.administrator.Username(), "remoteIP", remoteIP(r), "records", previousCount)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}
