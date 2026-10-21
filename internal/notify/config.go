// Package notify 保存告警邮件通知配置与告警记录。
//
// 配置与记录都落在数据目录里的单个 JSON 文件：管理员在「邮箱配置」页维护 SMTP 与收件人，
// 在「流量监控」页维护阈值与检测周期。写文件走临时文件 + rename，避免进程中断留下半个 JSON。
package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"ctm/internal/mailer"
)

// 检测周期上下限（秒）。下限避免过密地打电信接口，上限避免周期长到失去告警意义。
const (
	MinIntervalSeconds = 5
	MaxIntervalSeconds = 60
	// DefaultMinIntervalSeconds / DefaultMaxIntervalSeconds 首次使用的默认周期区间。
	DefaultMinIntervalSeconds = 5
	DefaultMaxIntervalSeconds = 10
)

// 告警记录保留条数：够回看近期触发情况，又不会让配置文件无限增长。
const maxRecords = 200

// Settings 邮件发送与流量告警配置（数据目录 notify.json）。
type Settings struct {
	// Enabled 是否启用流量告警；关闭时仍按周期采集，只是不发送邮件。
	Enabled bool `json:"enabled"`
	// Threshold 通用流量剩余额度告警阈值（字节）；剩余量低于它时告警。
	Threshold int64 `json:"threshold"`
	// IntervalMinSeconds / IntervalMaxSeconds 检测周期区间（秒），每轮在区间内取随机值。
	IntervalMinSeconds int `json:"intervalMinSeconds"`
	IntervalMaxSeconds int `json:"intervalMaxSeconds"`
	// Recipients 告警收件人；目前只有流量告警使用，与发信配置分开保存。
	Recipients []string `json:"recipients"`
	// Config 发信配置，不含收件人。
	mailer.Config
	// UpdatedAt 配置最后更新时间（Unix 秒）。
	UpdatedAt int64 `json:"updatedAt"`
	// Records 最近告警记录，按时间倒序保存。
	Records []Record `json:"records"`
	// ThresholdApplied 上次判断告警时生效的阈值。
	// 阈值被改动后，即使剩余量没有继续下降也视为新的一次触发，立即提醒。
	ThresholdApplied int64 `json:"thresholdApplied,omitempty"`
	// AlertedAt 当前这段「低于阈值」状态里最近一次成功提醒时间（Unix 秒）。
	// 落盘保存，避免进程重启后重复发信。
	AlertedAt int64 `json:"alertedAt,omitempty"`
	// Armed 是否处于「尚未提醒」状态；触发提醒后置 false，回到阈值以上或未发送成功时置 true。
	// 落盘保存，重启后沿用上一轮状态。
	Armed bool `json:"armed"`
}

// Record 一次告警尝试的结果快照。
type Record struct {
	// At 触发时间（Unix 秒）。
	At int64 `json:"at"`
	// Remaining 触发时的剩余通用流量（字节）。
	Remaining int64 `json:"remaining"`
	// Threshold 触发时使用的阈值（字节）。
	Threshold int64 `json:"threshold"`
	// Recipients 本次投递的收件人。
	Recipients []string `json:"recipients"`
	// Reason 触发原因：threshold（跌破阈值）/ thresholdChanged（阈值调整后仍不足）。
	Reason string `json:"reason,omitempty"`
	// Success 是否投递成功。
	Success bool `json:"success"`
	// Error 投递失败原因；成功时为空。
	Error string `json:"error,omitempty"`
}

// 告警触发原因。
const (
	// ReasonBelowThreshold 剩余量跌破阈值。
	ReasonBelowThreshold = "threshold"
	// ReasonThresholdChanged 阈值被调整，且当前剩余量仍低于新阈值。
	ReasonThresholdChanged = "thresholdChanged"
)

// Validate 校验配置能否直接用于告警：发信配置、收件人、阈值与周期都必须完整。
func (s Settings) Validate() error {
	if err := s.ValidateSender(); err != nil {
		return err
	}
	if err := mailer.ValidateRecipients(s.Recipients); err != nil {
		return err
	}
	if s.Threshold <= 0 {
		return errors.New("「剩余多少 GB 时提醒」要填一个大于 0 的数字")
	}
	return ValidateInterval(s.IntervalMinSeconds, s.IntervalMaxSeconds)
}

// ValidateInterval 校验检查间隔：两个数字都要在 5-60 秒之间，且「最多」不小于「最少」。
func ValidateInterval(minSeconds, maxSeconds int) error {
	if minSeconds < MinIntervalSeconds || minSeconds > MaxIntervalSeconds {
		return fmt.Errorf("「最少隔几秒查一次」要在 %d-%d 秒之间", MinIntervalSeconds, MaxIntervalSeconds)
	}
	if maxSeconds < MinIntervalSeconds || maxSeconds > MaxIntervalSeconds {
		return fmt.Errorf("「最多隔几秒查一次」要在 %d-%d 秒之间", MinIntervalSeconds, MaxIntervalSeconds)
	}
	if maxSeconds < minSeconds {
		return errors.New("「最多隔几秒查一次」不能比「最少隔几秒查一次」小")
	}
	return nil
}

// Store 配置文件读写句柄。
type Store struct {
	// mu 串行化读写，避免告警协程与 Web 请求同时写同一个文件。
	mu   sync.Mutex
	path string
}

// NewStore 创建配置存储；path 为数据目录下的 notify.json。
func NewStore(path string) *Store { return &Store{path: path} }

// Path 配置文件路径。
func (s *Store) Path() string { return s.path }

// Load 读取配置；文件不存在时返回带默认周期的空配置。
func (s *Store) Load() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// loadLocked 读取配置，调用方需持有锁。
func (s *Store) loadLocked() (Settings, error) {
	settings := defaultSettings()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return settings, nil
		}
		return settings, fmt.Errorf("读取通知配置: %w", err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return settings, nil
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return settings, fmt.Errorf("解析通知配置: %w", err)
	}
	// 周期区间缺失（老配置或手写文件）时回落到默认值，避免 @ 区间为空
	if settings.IntervalMinSeconds <= 0 {
		settings.IntervalMinSeconds = DefaultMinIntervalSeconds
	}
	if settings.IntervalMaxSeconds <= 0 {
		settings.IntervalMaxSeconds = DefaultMaxIntervalSeconds
	}
	if settings.IntervalMaxSeconds < settings.IntervalMinSeconds {
		settings.IntervalMaxSeconds = settings.IntervalMinSeconds
	}
	return settings, nil
}

// defaultSettings 首次使用的默认配置：周期 5-10 秒，其余留空待管理员填写。
func defaultSettings() Settings {
	return Settings{
		IntervalMinSeconds: DefaultMinIntervalSeconds,
		IntervalMaxSeconds: DefaultMaxIntervalSeconds,
	}
}

// Save 原子写入配置。
func (s *Store) Save(settings Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(settings)
}

// Update 在同一把锁内完成「读取 → 修改 → 写回」，避免并发写覆盖别的字段。
// mutate 返回的 Settings 会被落盘；返回 false 表示无需写入。
func (s *Store) Update(mutate func(*Settings) bool) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	settings, err := s.loadLocked()
	if err != nil {
		return settings, err
	}
	if !mutate(&settings) {
		return settings, nil
	}
	if err := s.saveLocked(settings); err != nil {
		return settings, err
	}
	return settings, nil
}

// saveLocked 原子写入，调用方需持有锁。
func (s *Store) saveLocked(settings Settings) error {
	if len(settings.Records) > maxRecords {
		settings.Records = settings.Records[:maxRecords]
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("创建配置目录: %w", err)
	}
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化通知配置: %w", err)
	}
	temp := s.path + ".tmp"
	// 配置里有 SMTP 密码，只给属主读写
	if err := os.WriteFile(temp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入通知配置: %w", err)
	}
	if err := os.Rename(temp, s.path); err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("替换通知配置: %w", err)
	}
	return nil
}

// AppendRecord 追加一条告警记录到文件头（最新在前）。
func (s *Store) AppendRecord(record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	settings, err := s.loadLocked()
	if err != nil {
		return err
	}
	records := make([]Record, 0, len(settings.Records)+1)
	records = append(records, record)
	records = append(records, settings.Records...)
	if len(records) > maxRecords {
		records = records[:maxRecords]
	}
	settings.Records = records
	return s.saveLocked(settings)
}
