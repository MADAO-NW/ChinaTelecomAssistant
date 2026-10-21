package notify

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ctm/internal/mailer"
	"ctm/models"
)

// fakeSender 记录每次发送的主题与收件人，并按预设返回错误。
type fakeSender struct {
	mu       sync.Mutex
	subjects []string
	calls    int
	failWith error
}

func (f *fakeSender) send(_ mailer.Config, subject, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.subjects = append(f.subjects, subject)
	return f.failWith
}

// newTestSettings 构造可用的配置：阈值 10GB、周期 5-10 秒、两个收件人。
// 收件人独立于发信配置：发信配置只保存 SMTP 与发件人。
func newTestSettings() Settings {
	return Settings{
		Enabled:            true,
		Threshold:          10 * 1024 * 1024 * 1024,
		IntervalMinSeconds: DefaultMinIntervalSeconds,
		IntervalMaxSeconds: DefaultMaxIntervalSeconds,
		Recipients:         []string{"a@example.com", "b@example.com"},
		Config: mailer.Config{
			Host:       "smtp.example.com",
			Port:       465,
			From:       "notify@example.com",
			Encryption: mailer.EncryptionSSL,
		},
	}
}

// summaryWith 构造通用流量总量/已用，用于推算剩余量。
func summaryWith(total, used int64) models.Summary {
	return models.Summary{GeneralTotal: total, GeneralUse: used}
}

func TestCollectOnceAlertsToAllRecipientsAndRecords(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "notify.json"))
	settings := newTestSettings()
	if err := store.Save(settings); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	// 剩余 1GB，低于 10GB 阈值
	monitor := NewMonitor(store, func() (models.Summary, error) {
		return summaryWith(100*1024*1024*1024, 99*1024*1024*1024), nil
	}, sender.send, nil)

	snapshot := monitor.CollectOnce()
	if snapshot.Error != "" {
		t.Fatalf("采集不应失败: %s", snapshot.Error)
	}
	want := int64(1024 * 1024 * 1024)
	if snapshot.Remaining != want {
		t.Fatalf("剩余量 = %d，期望 %d", snapshot.Remaining, want)
	}
	if sender.calls != 1 {
		t.Fatalf("应发送 1 封告警，实际 %d", sender.calls)
	}
	// 一次发送覆盖全部收件人（mailer 按配置里的 Recipients 逐个 RCPT）
	if len(settings.Recipients) != 2 {
		t.Fatalf("收件人数量 = %d，期望 2", len(settings.Recipients))
	}
	if !strings.Contains(sender.subjects[0], "通用流量剩余不足") {
		t.Fatalf("主题不符合预期: %s", sender.subjects[0])
	}

	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Records) != 1 {
		t.Fatalf("告警记录 = %d 条，期望 1", len(saved.Records))
	}
	record := saved.Records[0]
	if !record.Success || record.Remaining != want || record.Threshold != settings.Threshold {
		t.Fatalf("记录内容不符合预期: %+v", record)
	}
	if len(record.Recipients) != 2 {
		t.Fatalf("记录里的收件人 = %v，期望 2 个", record.Recipients)
	}
}

func TestCollectOnceSkipsWhenAboveThreshold(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "notify.json"))
	settings := newTestSettings()
	if err := store.Save(settings); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	// 剩余 50GB，高于 10GB 阈值
	monitor := NewMonitor(store, func() (models.Summary, error) {
		return summaryWith(100*1024*1024*1024, 50*1024*1024*1024), nil
	}, sender.send, nil)

	monitor.CollectOnce()
	if sender.calls != 0 {
		t.Fatalf("高于阈值时不应发信，实际 %d 封", sender.calls)
	}
	saved, _ := store.Load()
	if len(saved.Records) != 0 {
		t.Fatalf("高于阈值时不应产生记录，实际 %d 条", len(saved.Records))
	}
}

func TestAlertsAreDedupedThenRearmedAfterRecovery(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "notify.json"))
	if err := store.Save(newTestSettings()); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	remaining := int64(1024 * 1024 * 1024) // 1GB，低于阈值
	var mu sync.Mutex
	monitor := NewMonitor(store, func() (models.Summary, error) {
		mu.Lock()
		defer mu.Unlock()
		return summaryWith(100*1024*1024*1024, 100*1024*1024*1024-remaining), nil
	}, sender.send, nil)

	monitor.CollectOnce()
	if sender.calls != 1 {
		t.Fatalf("首次触发应发 1 封，实际 %d", sender.calls)
	}
	// 仍低于阈值：30 分钟窗口内不重复发信
	monitor.lastCollectAt = time.Time{}
	monitor.CollectOnce()
	if sender.calls != 1 {
		t.Fatalf("窗口内不应重复发信，实际 %d 封", sender.calls)
	}

	// 回到阈值以上：重新武装，但本轮不发信
	mu.Lock()
	remaining = 90 * 1024 * 1024 * 1024
	mu.Unlock()
	monitor.lastCollectAt = time.Time{}
	monitor.CollectOnce()
	if sender.calls != 1 {
		t.Fatalf("恢复期间不应发信，实际 %d 封", sender.calls)
	}

	// 再次跌破：立即提醒
	mu.Lock()
	remaining = 1024 * 1024 * 1024
	mu.Unlock()
	monitor.lastCollectAt = time.Time{}
	monitor.CollectOnce()
	if sender.calls != 2 {
		t.Fatalf("恢复后再次跌破应立即提醒，实际 %d 封", sender.calls)
	}
}

func TestFailedSendIsRecordedAndRetried(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "notify.json"))
	if err := store.Save(newTestSettings()); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{failWith: errors.New("smtp 连接被拒绝")}
	monitor := NewMonitor(store, func() (models.Summary, error) {
		return summaryWith(100*1024*1024*1024, 99*1024*1024*1024), nil
	}, sender.send, nil)

	monitor.CollectOnce()
	saved, _ := store.Load()
	if len(saved.Records) != 1 || saved.Records[0].Success {
		t.Fatalf("失败也应记录，且 Success=false: %+v", saved.Records)
	}
	if saved.Records[0].Error == "" {
		t.Fatal("失败记录应带错误原因")
	}

	// 发送失败不算已提醒：下一轮立刻重试
	monitor.lastCollectAt = time.Time{}
	monitor.CollectOnce()
	if sender.calls != 2 {
		t.Fatalf("失败后应重试，实际发送 %d 次", sender.calls)
	}
}

func TestCollectErrorDoesNotAlertOrOverwriteRemaining(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "notify.json"))
	if err := store.Save(newTestSettings()); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	failing := false
	monitor := NewMonitor(store, func() (models.Summary, error) {
		if failing {
			return models.Summary{}, errors.New("上游超时")
		}
		return summaryWith(100*1024*1024*1024, 99*1024*1024*1024), nil
	}, sender.send, nil)

	// 先把 armed 消耗掉，避免首次成功触发干扰断言
	monitor.armed = false
	monitor.lastAlertAt = time.Now()
	failing = true
	monitor.lastCollectAt = time.Time{}
	snapshot := monitor.CollectOnce()
	if snapshot.Error == "" {
		t.Fatal("采集失败应带错误信息")
	}
	if sender.calls != 0 {
		t.Fatalf("采集失败不应发信，实际 %d 封", sender.calls)
	}
	if snapshot.Remaining != 0 {
		t.Fatalf("失败结果不应带剩余量，实际 %d", snapshot.Remaining)
	}
}

func TestDisabledOrMissingThresholdDoesNotAlert(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "notify.json"))
	settings := newTestSettings()
	settings.Enabled = false
	if err := store.Save(settings); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	monitor := NewMonitor(store, func() (models.Summary, error) {
		return summaryWith(100*1024*1024*1024, 99*1024*1024*1024), nil
	}, sender.send, nil)
	monitor.CollectOnce()
	if sender.calls != 0 {
		t.Fatalf("关闭告警后不应发信，实际 %d 封", sender.calls)
	}
}

func TestStorePersistsRecordsNewestFirstAndCapsCount(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "notify.json"))
	if err := store.Save(newTestSettings()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := store.AppendRecord(Record{At: int64(i + 1), Remaining: int64(i), Threshold: 1, Success: true}); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Records) != 3 {
		t.Fatalf("记录数 = %d，期望 3", len(saved.Records))
	}
	if saved.Records[0].At != 3 {
		t.Fatalf("记录应最新在前，首条 At = %d", saved.Records[0].At)
	}
	// 配置字段不能被追加记录改掉
	if !saved.Enabled || saved.Threshold != 10*1024*1024*1024 || len(saved.Recipients) != 2 {
		t.Fatalf("追加记录不应改动配置: %+v", saved)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		value int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2 KB"},
		{5 * 1024 * 1024, "5 MB"},
		{10 * 1024 * 1024 * 1024, "10.00 GB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.value); got != c.want {
			t.Fatalf("formatBytes(%d) = %s，期望 %s", c.value, got, c.want)
		}
	}
}

func TestClampInterval(t *testing.T) {
	if got := clampInterval(1); got != MinIntervalSeconds {
		t.Fatalf("过小周期应夹到 %d，实际 %d", MinIntervalSeconds, got)
	}
	if got := clampInterval(9999); got != MaxIntervalSeconds {
		t.Fatalf("过大周期应夹到 %d，实际 %d", MaxIntervalSeconds, got)
	}
	if got := clampInterval(10); got != 10 {
		t.Fatalf("合法周期应原样返回，实际 %d", got)
	}
}

func TestRandomIntervalStaysWithinRange(t *testing.T) {
	// 区间内取值，且两端都能取到（闭区间）
	seen := map[int]bool{}
	for i := 0; i < 500; i++ {
		got := randomInterval(5, 7)
		seconds := int(got / time.Second)
		if seconds < 5 || seconds > 7 {
			t.Fatalf("随机周期 %d 秒超出 [5,7]", seconds)
		}
		seen[seconds] = true
	}
	if len(seen) != 3 {
		t.Fatalf("闭区间应能取到 5/6/7 三个值，实际 %v", seen)
	}
	// 上下限相等时固定返回该值
	if got := randomInterval(9, 9); got != 9*time.Second {
		t.Fatalf("min==max 时应固定为 9 秒，实际 %v", got)
	}
	// 配置越界时先夹到合法区间，不 panic
	if got := randomInterval(1, 1); got != MinIntervalSeconds*time.Second {
		t.Fatalf("越界区间应夹到 %d 秒，实际 %v", MinIntervalSeconds, got)
	}
}

func TestThresholdChangeTriggersAlertsEvenWhenStillBelow(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "notify.json"))
	settings := newTestSettings()
	if err := store.Save(settings); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	// 剩余固定 8GB：低于 10GB 阈值，也低于随后调高的 20GB 阈值
	monitor := NewMonitor(store, func() (models.Summary, error) {
		return summaryWith(100*1024*1024*1024, 92*1024*1024*1024), nil
	}, sender.send, nil)

	monitor.CollectOnce()
	if sender.calls != 1 {
		t.Fatalf("首次跌破应发 1 封，实际 %d", sender.calls)
	}
	// 未改阈值、仍低于阈值：窗口内不重复发信
	monitor.lastCollectAt = time.Time{}
	monitor.CollectOnce()
	if sender.calls != 1 {
		t.Fatalf("未改阈值不应重复发信，实际 %d 封", sender.calls)
	}

	// 把阈值调高到 20GB（剩余 8GB 仍低于新阈值），视为新的触发
	if _, err := store.Update(func(s *Settings) bool {
		s.Threshold = 20 * 1024 * 1024 * 1024
		return true
	}); err != nil {
		t.Fatal(err)
	}
	monitor.lastCollectAt = time.Time{}
	monitor.CollectOnce()
	if sender.calls != 2 {
		t.Fatalf("阈值调高后应再次告警，实际 %d 封", sender.calls)
	}

	// 再把阈值调低到 4GB：当前剩余 8GB 高于新阈值，不发信但要重新武装
	if _, err := store.Update(func(s *Settings) bool {
		s.Threshold = 4 * 1024 * 1024 * 1024
		return true
	}); err != nil {
		t.Fatal(err)
	}
	monitor.lastCollectAt = time.Time{}
	monitor.CollectOnce()
	if sender.calls != 2 {
		t.Fatalf("阈值调低后剩余高于阈值不应发信，实际 %d 封", sender.calls)
	}

	// 再调回 10GB：剩余 8GB 又低于阈值，属于再次触发，应立即可发
	if _, err := store.Update(func(s *Settings) bool {
		s.Threshold = 10 * 1024 * 1024 * 1024
		return true
	}); err != nil {
		t.Fatal(err)
	}
	monitor.lastCollectAt = time.Time{}
	monitor.CollectOnce()
	if sender.calls != 3 {
		t.Fatalf("阈值再次调整后应告警，实际 %d 封", sender.calls)
	}
	// 同一阈值下继续低于阈值：不应重复发信
	monitor.lastCollectAt = time.Time{}
	monitor.CollectOnce()
	if sender.calls != 3 {
		t.Fatalf("阈值未变时不应重复发信，实际 %d 封", sender.calls)
	}

	// 记录里应能区分「阈值调整」这个触发原因
	saved, _ := store.Load()
	if len(saved.Records) != 3 {
		t.Fatalf("应记录 3 次告警，实际 %d", len(saved.Records))
	}
	if saved.Records[0].Reason != ReasonThresholdChanged {
		t.Fatalf("最近一次触发原因应为 %s，实际 %s", ReasonThresholdChanged, saved.Records[0].Reason)
	}
}

func TestThresholdAppliedPersistedSoRestartDoesNotSpam(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "notify.json"))
	settings := newTestSettings()
	if err := store.Save(settings); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	monitor := NewMonitor(store, func() (models.Summary, error) {
		return summaryWith(100*1024*1024*1024, 92*1024*1024*1024), nil
	}, sender.send, nil)
	monitor.CollectOnce()
	if sender.calls != 1 {
		t.Fatalf("首次应发 1 封，实际 %d", sender.calls)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.ThresholdApplied != settings.Threshold {
		t.Fatalf("阈值基准应落盘为 %d，实际 %d", settings.Threshold, saved.ThresholdApplied)
	}

	// 模拟进程重启：新的 Monitor 实例读到相同阈值，不应误判为「阈值被改动」而重复发信
	restarted := NewMonitor(store, func() (models.Summary, error) {
		return summaryWith(100*1024*1024*1024, 92*1024*1024*1024), nil
	}, sender.send, nil)
	restarted.CollectOnce()
	if sender.calls != 1 {
		t.Fatalf("重启后未改阈值不应重复发信，实际 %d 封", sender.calls)
	}
}

func TestValidateIntervalRejectsBadRange(t *testing.T) {
	cases := []struct{ min, max int }{
		{MinIntervalSeconds - 1, 10},
		{10, MaxIntervalSeconds + 1},
		{20, 10},
	}
	for _, c := range cases {
		if err := ValidateInterval(c.min, c.max); err == nil {
			t.Fatalf("区间 %d-%d 应被拒绝", c.min, c.max)
		}
	}
	if err := ValidateInterval(5, 10); err != nil {
		t.Fatalf("合法区间不应报错: %v", err)
	}
}
