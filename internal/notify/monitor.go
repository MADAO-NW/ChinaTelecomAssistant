package notify

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"ctm/internal/mailer"
	"ctm/models"
)

// 常量集中在这里，便于和前端文案、配置校验保持一致。
const (
	// subjectPrefix 告警邮件主题前缀，便于收件人过滤。
	subjectPrefix = "【电信助手】"
	// alertRepeatWindow 同一轮「剩余量持续低于阈值」状态下的最小重复提醒间隔：
	// 触发后 30 分钟内不再重复发信，避免每轮都刷屏；恢复到阈值以上后重新武装，再次跌破会立刻提醒。
	alertRepeatWindow = 30 * time.Minute
	// minCollectGap 两次采集之间的最小间隔，避免手动刷新与定时采集叠加打爆上游接口。
	minCollectGap = 3 * time.Second
)

// Snapshot 一次采集的结果，供页面展示「最近一次检测」。
type Snapshot struct {
	// At 采集完成时间（Unix 秒）。
	At int64 `json:"at"`
	// Remaining 通用流量剩余量（字节）。
	Remaining int64 `json:"remaining"`
	// Total 通用流量总量（字节）。
	Total int64 `json:"total"`
	// Error 采集失败原因；成功时为空。
	Error string `json:"error,omitempty"`
}

// Monitor 流量告警监控：按配置周期采集通用流量，剩余量跌破阈值时给所有收件人发邮件。
type Monitor struct {
	// store 配置与记录存储。
	store *Store
	// collect 采集一次用量快照；由 Web 层注入，复用已有的电信网关与刷新逻辑。
	collect func() (models.Summary, error)
	// send 发送邮件；默认 mailer.Send，测试可替换。
	send func(cfg mailer.Config, subject, body string) error
	// logger 记录告警过程。
	logger Logger

	// mu 保护下面这些运行时状态。
	mu sync.Mutex
	// running 当前是否已有采集在跑，避免重入。
	running bool
	// lastCollectAt 最近一次采集完成时间。
	lastCollectAt time.Time
	// lastAlertAt 「连续低于阈值」这段状态里最近一次成功发信的时间（内存镜像，落盘在 Settings.AlertedAt）。
	lastAlertAt time.Time
	// armed 是否处于「尚未提醒过」的状态（内存镜像，落盘在 Settings.Armed）。
	armed bool
	// snapshot 最近一次采集结果。
	snapshot Snapshot
	// nextRunAt 下一轮定时采集时间。
	nextRunAt time.Time

	// stopped 关闭信号，Shutdown 时结束采集协程。
	stopped  chan struct{}
	stopOnce sync.Once
}

// Logger 监控只依赖 Info/Warn/Error 三档日志，避免把 zap 类型漏进这个包。
type Logger interface {
	Infof(template string, args ...any)
	Warnf(template string, args ...any)
	Errorf(template string, args ...any)
}

// NewMonitor 创建监控器。collect 必须是非阻塞的采集函数，send 为 nil 时用 mailer.Send。
// 告警去重状态从配置文件恢复，进程重启后不会因为状态清零而重复发信。
func NewMonitor(store *Store, collect func() (models.Summary, error), send func(mailer.Config, string, string) error, logger Logger) *Monitor {
	if send == nil {
		send = mailer.Send
	}
	monitor := &Monitor{
		store:   store,
		collect: collect,
		send:    send,
		logger:  logger,
		armed:   true,
		stopped: make(chan struct{}),
	}
	if settings, err := store.Load(); err == nil {
		monitor.armed = settings.Armed || settings.AlertedAt == 0
		if settings.AlertedAt > 0 {
			monitor.lastAlertAt = time.Unix(settings.AlertedAt, 0)
		}
	}
	return monitor
}

// Start 启动后台采集协程；重复调用无副作用。
func (m *Monitor) Start() {
	go m.loop()
}

// Shutdown 停止采集协程并等待当前一轮结束。
func (m *Monitor) Shutdown() {
	m.stopOnce.Do(func() { close(m.stopped) })
}

// Status 返回页面展示用的运行状态。
func (m *Monitor) Status() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := map[string]any{
		"lastCollectAt": m.lastCollectAt.Unix(),
		"nextRunAt":     m.nextRunAt.Unix(),
		"armed":         m.armed,
		"remaining":     m.snapshot.Remaining,
		"total":         m.snapshot.Total,
	}
	if m.snapshot.At > 0 {
		status["lastCollectError"] = m.snapshot.Error
	}
	if !m.lastAlertAt.IsZero() {
		status["lastAlertAt"] = m.lastAlertAt.Unix()
	}
	return status
}

// loop 按配置周期采集；周期每次重新读取，改配置后无需重启。
func (m *Monitor) loop() {
	for {
		settings, err := m.store.Load()
		if err != nil {
			m.logf("warn", "读取流量告警配置失败，本轮跳过: %v", err)
		}
		wait := randomInterval(settings.IntervalMinSeconds, settings.IntervalMaxSeconds)
		m.mu.Lock()
		m.nextRunAt = time.Now().Add(wait)
		m.mu.Unlock()
		select {
		case <-m.stopped:
			return
		case <-time.After(wait):
		}
		m.CollectOnce()
	}
}

// randomInterval 在 [minSeconds, maxSeconds] 区间内取一个随机周期；配置越界时先夹到合法范围。
func randomInterval(minSeconds, maxSeconds int) time.Duration {
	lo, hi := clampInterval(minSeconds), clampInterval(maxSeconds)
	if hi < lo {
		lo, hi = hi, lo
	}
	if hi == lo {
		return time.Duration(lo) * time.Second
	}
	// [lo, hi] 闭区间，避免上限永远取不到
	seconds := lo + rand.Intn(hi-lo+1)
	return time.Duration(seconds) * time.Second
}

// clampInterval 把配置里的周期夹到允许区间。
func clampInterval(seconds int) int {
	if seconds < MinIntervalSeconds {
		return MinIntervalSeconds
	}
	if seconds > MaxIntervalSeconds {
		return MaxIntervalSeconds
	}
	return seconds
}

// CollectOnce 定时采集一轮并按需告警；与上一次采集过近时退让，避免叠加打爆上游接口。
func (m *Monitor) CollectOnce() Snapshot {
	return m.runCollect(false)
}

// CollectNow 手动采集一轮：跳过最小间隔检查，用户点「立即检测」时总能拿到新数据。
// 仍受「已有采集在跑」保护，不会并发打上游。
func (m *Monitor) CollectNow() Snapshot {
	return m.runCollect(true)
}

// runCollect 执行一轮采集；force 为真时忽略最小间隔。
func (m *Monitor) runCollect(force bool) Snapshot {
	m.mu.Lock()
	if m.running {
		snapshot := m.snapshot
		m.mu.Unlock()
		return snapshot
	}
	// 定时采集与手动检测叠加时，退让给正在进行的那一轮
	if !force {
		if gap := time.Since(m.lastCollectAt); !m.lastCollectAt.IsZero() && gap < minCollectGap {
			snapshot := m.snapshot
			m.mu.Unlock()
			return snapshot
		}
	}
	// 从这里开始独占一轮采集：先占位再解锁，避免后续请求重入
	m.running = true
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.running = false
		m.lastCollectAt = time.Now()
		m.mu.Unlock()
	}()

	summary, err := m.collect()
	snapshot := Snapshot{At: time.Now().Unix()}
	if err != nil {
		snapshot.Error = err.Error()
		m.mu.Lock()
		m.snapshot = snapshot
		m.mu.Unlock()
		m.logf("warn", "流量采集失败，本轮不判断告警: %v", err)
		return snapshot
	}
	remaining := summary.GeneralTotal - summary.GeneralUse
	if remaining < 0 {
		remaining = 0
	}
	snapshot.Remaining = remaining
	snapshot.Total = summary.GeneralTotal
	m.mu.Lock()
	m.snapshot = snapshot
	m.mu.Unlock()

	settings, loadErr := m.store.Load()
	if loadErr != nil {
		m.logf("error", "读取流量告警配置失败，跳过告警判断: %v", loadErr)
		return snapshot
	}
	if !settings.Enabled || settings.Threshold <= 0 {
		return snapshot
	}
	// 阈值被改动：无论调高还是调低都视作一次新的触发，只要当前剩余量仍低于新阈值就立即提醒。
	// ThresholdApplied == 0 表示还没有基线（首次运行），只建立基准，不算「被改动」。
	thresholdChanged := settings.ThresholdApplied != 0 && settings.ThresholdApplied != settings.Threshold
	if settings.ThresholdApplied != settings.Threshold {
		if thresholdChanged {
			m.mu.Lock()
			m.armed = true
			m.mu.Unlock()
		}
		m.persistThreshold(settings.Threshold)
	}
	if remaining > settings.Threshold {
		// 回到阈值以上：重新武装，下次跌破立即提醒
		m.mu.Lock()
		needsRearm := !m.armed
		if needsRearm {
			m.armed = true
		}
		m.mu.Unlock()
		if needsRearm {
			m.logf("info", "通用流量剩余已回到阈值以上，恢复告警监测（剩余 %d 字节，阈值 %d 字节）", remaining, settings.Threshold)
			// 重新武装要落盘，否则重启后又按「已提醒」状态继续沉默
			if _, err := m.store.Update(func(s *Settings) bool {
				s.Armed = true
				return true
			}); err != nil {
				m.logf("error", "记录告警监测状态失败: %v", err)
			}
		}
		return snapshot
	}
	reason := ReasonBelowThreshold
	if thresholdChanged {
		reason = ReasonThresholdChanged
	}
	m.alert(settings, remaining, snapshot.At, reason)
	return snapshot
}

// persistThreshold 记录本次判断使用的阈值，作为下一轮判断「阈值是否被改动」的基准。
func (m *Monitor) persistThreshold(threshold int64) {
	if _, err := m.store.Update(func(s *Settings) bool {
		if s.ThresholdApplied == threshold {
			return false
		}
		s.ThresholdApplied = threshold
		return true
	}); err != nil {
		m.logf("error", "记录告警阈值基准失败: %v", err)
	}
}

// alert 发一封告警邮件并写一条记录；同一段「低于阈值」状态里按最小间隔去重。
// 去重状态（Armed / AlertedAt）与记录都落盘，进程重启后不会重复发信、也不会丢记录。
func (m *Monitor) alert(settings Settings, remaining int64, at int64, reason string) {
	m.mu.Lock()
	// 已在「低于阈值」状态且未过最小重复间隔：本轮不发，等窗口过去或恢复后重新武装
	if !m.armed && time.Since(m.lastAlertAt) < alertRepeatWindow {
		m.mu.Unlock()
		return
	}
	m.armed = false
	m.mu.Unlock()

	subject, body := alertMail(remaining, settings.Threshold, settings.Recipients, reason, time.Unix(at, 0))
	err := m.send(withRecipients(settings.Config, settings.Recipients), subject, body)
	record := Record{
		At:         at,
		Remaining:  remaining,
		Threshold:  settings.Threshold,
		Recipients: append([]string(nil), settings.Recipients...),
		Reason:     reason,
		Success:    err == nil,
	}
	now := time.Now()
	if err != nil {
		record.Error = err.Error()
		m.logf("error", "流量告警邮件发送失败（剩余 %d 字节，阈值 %d 字节，收件人 %s）: %v",
			remaining, settings.Threshold, strings.Join(settings.Recipients, "、"), err)
		// 发送失败不算已提醒：重置提醒时间并重新武装，下一轮立刻重试
		m.mu.Lock()
		m.lastAlertAt = time.Time{}
		m.armed = true
		m.mu.Unlock()
	} else {
		m.mu.Lock()
		m.lastAlertAt = now
		m.mu.Unlock()
		m.logf("warn", "通用流量剩余低于阈值，已发送告警邮件（剩余 %d 字节，阈值 %d 字节，原因 %s，收件人 %s）",
			remaining, settings.Threshold, reason, strings.Join(settings.Recipients, "、"))
	}
	// 一次写入同时更新去重状态与记录，避免两次写文件之间被中断导致状态与记录不一致
	if _, updateErr := m.store.Update(func(s *Settings) bool {
		if err != nil {
			s.Armed = true
			s.AlertedAt = 0
		} else {
			s.Armed = false
			s.AlertedAt = now.Unix()
		}
		records := make([]Record, 0, len(s.Records)+1)
		records = append(records, record)
		records = append(records, s.Records...)
		s.Records = records
		return true
	}); updateErr != nil {
		m.logf("error", "写入流量告警记录失败: %v", updateErr)
	}
}

// withRecipients 把告警收件人放进发信配置：mailer 按 Config.Recipients 逐个 RCPT TO。
func withRecipients(cfg mailer.Config, recipients []string) mailer.Config {
	cfg.Recipients = append([]string(nil), recipients...)
	return cfg
}

// logf 按等级输出日志；logger 为 nil 时静默，便于测试。
func (m *Monitor) logf(level, format string, args ...any) {
	if m.logger == nil {
		return
	}
	switch level {
	case "warn":
		m.logger.Warnf(format, args...)
	case "error":
		m.logger.Errorf(format, args...)
	default:
		m.logger.Infof(format, args...)
	}
}

// alertMail 组装告警邮件主题与正文。
func alertMail(remaining, threshold int64, recipients []string, reason string, now time.Time) (string, string) {
	subject := fmt.Sprintf("%s通用流量剩余不足：%s", subjectPrefix, formatBytes(remaining))
	var builder strings.Builder
	fmt.Fprintf(&builder, "检测时间：%s\n", now.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&builder, "通用流量剩余：%s\n", formatBytes(remaining))
	fmt.Fprintf(&builder, "告警阈值：%s\n", formatBytes(threshold))
	if reason == ReasonThresholdChanged {
		builder.WriteString("触发原因：告警阈值已调整，当前剩余量仍低于新阈值\n")
	} else {
		builder.WriteString("触发原因：通用流量剩余已跌破阈值\n")
	}
	fmt.Fprintf(&builder, "收件人：%s\n\n", strings.Join(recipients, "、"))
	builder.WriteString("请及时关注用量或调整套餐。\n")
	builder.WriteString("剩余量回到阈值以上后会自动恢复监测，再次跌破会重新提醒。\n")
	return subject, builder.String()
}

// formatBytes 把字节数格式化成易读的流量文本，和页面上其他位置的口径一致（1GB = 1024MB）。
func formatBytes(value int64) string {
	if value < 0 {
		value = 0
	}
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case value >= gb:
		return fmt.Sprintf("%.2f GB", float64(value)/float64(gb))
	case value >= mb:
		return fmt.Sprintf("%.0f MB", float64(value)/float64(mb))
	case value >= kb:
		return fmt.Sprintf("%.0f KB", float64(value)/float64(kb))
	default:
		return fmt.Sprintf("%d B", value)
	}
}

// TestMail 用「页面上当前填写的发信配置」给指定收件人发一封测试邮件。
// 只用于验证填写是否正确，不读写已保存的配置、也不产生告警记录。
// recipients 为空时回落到已保存的告警收件人。
func (m *Monitor) TestMail(sender mailer.Config, recipients []string) ([]string, error) {
	// 密码没填（页面上留空表示沿用已保存的密码）时用已保存的密码补齐
	if sender.Password == "" {
		if saved, err := m.store.Load(); err == nil {
			sender.Password = saved.Password
		}
	}
	if err := sender.ValidateSender(); err != nil {
		return nil, err
	}
	if len(recipients) == 0 {
		saved, err := m.store.Load()
		if err != nil {
			return nil, err
		}
		recipients = saved.Recipients
	}
	if err := mailer.ValidateRecipients(recipients); err != nil {
		return nil, err
	}
	subject := subjectPrefix + "测试邮件"
	body := fmt.Sprintf("这是一封测试邮件，收到说明邮件通知可以用。\n\n发送时间：%s\n收件人：%s\n",
		time.Now().Format("2006-01-02 15:04:05"), strings.Join(recipients, "、"))
	if err := m.send(withRecipients(sender, recipients), subject, body); err != nil {
		return nil, err
	}
	return recipients, nil
}
