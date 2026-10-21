package notify

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ctm/internal/mailer"
)

// captureSender 记录一次发送实际使用的发信配置与收件人。
type captureSender struct {
	mu        sync.Mutex
	calls     int
	cfg       mailer.Config
	subject   string
	body      string
	failWith  error
	lastPlain string
}

func (c *captureSender) send(cfg mailer.Config, subject, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.cfg = cfg
	c.subject = subject
	c.body = body
	c.lastPlain = strings.Join(cfg.Recipients, ",")
	return c.failWith
}

func TestTestMailUsesUnsavedConfigAndDoesNotPersist(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "notify.json"))
	saved := newTestSettings()
	saved.Password = "saved-password"
	if err := store.Save(saved); err != nil {
		t.Fatal(err)
	}
	sender := &captureSender{}
	monitor := NewMonitor(store, nil, sender.send, nil)

	// 页面上当前填写、尚未保存的一套配置：换服务器、换发件人、密码留空（沿用已保存的）
	pending := mailer.Config{
		Host:       "smtp.pending.test",
		Port:       587,
		From:       "pending@example.com",
		Encryption: mailer.EncryptionStartTLS,
	}
	sent, err := monitor.TestMail(pending, []string{"tester@example.com"})
	if err != nil {
		t.Fatalf("测试邮件不应失败: %v", err)
	}
	if len(sent) != 1 || sent[0] != "tester@example.com" {
		t.Fatalf("测试收件人 = %v，期望只发给 tester@example.com", sent)
	}
	// 发送必须用页面上填的那套，而不是已保存的那套
	if sender.cfg.Host != "smtp.pending.test" || sender.cfg.Port != 587 || sender.cfg.From != "pending@example.com" {
		t.Fatalf("实际使用的发信配置 = %+v，期望用当前填写的值", sender.cfg)
	}
	// 密码留空时应沿用已保存的密码
	if sender.cfg.Password != "saved-password" {
		t.Fatalf("密码应为已保存的密码，实际 %q", sender.cfg.Password)
	}
	// 收件人必须是本次传入的测试收件人，不能串到告警收件人
	if sender.lastPlain != "tester@example.com" {
		t.Fatalf("实际收件人 = %s，期望 tester@example.com", sender.lastPlain)
	}

	// 测试不能改动已保存的配置
	after, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.Host == "smtp.pending.test" || after.From == "pending@example.com" {
		t.Fatalf("测试邮件不应写入配置: %+v", after.Config)
	}
	if len(after.Records) != 0 {
		t.Fatalf("测试邮件不应产生告警记录，实际 %d 条", len(after.Records))
	}
	if len(after.Recipients) != 2 {
		t.Fatalf("告警收件人不应被测试改动: %v", after.Recipients)
	}
}

func TestTestMailFallsBackToSavedRecipients(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "notify.json"))
	if err := store.Save(newTestSettings()); err != nil {
		t.Fatal(err)
	}
	sender := &captureSender{}
	monitor := NewMonitor(store, nil, sender.send, nil)
	// 不填测试收件人时，用已保存的告警收件人
	sent, err := monitor.TestMail(mailer.Config{
		Host: "smtp.example.com", Port: 465, From: "notify@example.com", Encryption: mailer.EncryptionSSL,
	}, nil)
	if err != nil {
		t.Fatalf("测试邮件不应失败: %v", err)
	}
	if len(sent) != 2 {
		t.Fatalf("应回落到 2 个已保存收件人，实际 %v", sent)
	}
}

func TestTestMailRejectsIncompleteInput(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "notify.json"))
	if err := store.Save(newTestSettings()); err != nil {
		t.Fatal(err)
	}
	sender := &captureSender{}
	monitor := NewMonitor(store, nil, sender.send, nil)

	// 缺发件人
	if _, err := monitor.TestMail(mailer.Config{Host: "smtp.example.com", Port: 465}, []string{"a@example.com"}); err == nil {
		t.Fatal("缺发件人时应报错")
	}
	// 收件人格式不对
	if _, err := monitor.TestMail(mailer.Config{
		Host: "smtp.example.com", Port: 465, From: "n@example.com", Encryption: mailer.EncryptionSSL,
	}, []string{"not-an-email"}); err == nil {
		t.Fatal("收件人格式不正确时应报错")
	}
	if sender.calls != 0 {
		t.Fatalf("校验失败时不应真的发信，实际发送 %d 次", sender.calls)
	}
}

func TestTestMailSurfacesSendError(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "notify.json"))
	if err := store.Save(newTestSettings()); err != nil {
		t.Fatal(err)
	}
	sender := &captureSender{failWith: errors.New("连接被拒绝")}
	monitor := NewMonitor(store, nil, sender.send, nil)
	_, err := monitor.TestMail(mailer.Config{
		Host: "smtp.example.com", Port: 465, From: "n@example.com", Encryption: mailer.EncryptionSSL,
	}, []string{"a@example.com"})
	if err == nil || !strings.Contains(err.Error(), "连接被拒绝") {
		t.Fatalf("应把发送错误透出，实际 %v", err)
	}
}
