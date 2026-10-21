package mailer

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP 极简 SMTP 服务器：只实现投递一封邮件需要的命令，用于离线验证发送链路。
type fakeSMTP struct {
	listener net.Listener
	// advertiseStartTLS 是否在 EHLO 里广告 STARTTLS（只通告关键字，不做真实握手）。
	advertiseStartTLS bool
	// rejectAuth 认证是否固定失败，用于验证认证失败必须终止投递。
	rejectAuth bool

	mu         sync.Mutex
	authUser   string
	mailFrom   string
	recipients []string
	data       string
}

// fakeSMTPOptions 假服务器的能力开关。
type fakeSMTPOptions struct {
	advertiseStartTLS bool
	rejectAuth        bool
}

// startFakeSMTP 启动本地假 SMTP 服务器并返回句柄（测试结束时自动关闭）。
func startFakeSMTP(t *testing.T, options fakeSMTPOptions) *fakeSMTP {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听本地 SMTP 端口失败: %v", err)
	}
	server := &fakeSMTP{listener: listener, advertiseStartTLS: options.advertiseStartTLS, rejectAuth: options.rejectAuth}
	go server.serve()
	t.Cleanup(func() { listener.Close() })
	return server
}

func (s *fakeSMTP) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTP) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	reply := func(line string) {
		_, _ = writer.WriteString(line + "\r\n")
		_ = writer.Flush()
	}
	reply("220 fake ESMTP")
	inData := false
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if inData {
			if trimmed == "." {
				inData = false
				s.mu.Lock()
				s.data = data.String()
				s.mu.Unlock()
				reply("250 2.0.0 queued")
				continue
			}
			data.WriteString(strings.TrimPrefix(trimmed, "..") + "\n")
			continue
		}
		command := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			reply("250-fake greets you")
			if s.advertiseStartTLS {
				reply("250-STARTTLS")
			}
			reply("250 AUTH PLAIN")
		case strings.HasPrefix(command, "AUTH PLAIN"):
			payload := strings.TrimSpace(trimmed[len("AUTH PLAIN"):])
			raw, _ := base64.StdEncoding.DecodeString(payload)
			parts := strings.Split(string(raw), "\x00")
			if len(parts) >= 3 {
				s.mu.Lock()
				s.authUser = parts[1]
				s.mu.Unlock()
			}
			if s.rejectAuth {
				reply("535 5.7.8 authentication credentials invalid")
				continue
			}
			reply("235 2.7.0 authenticated")
		case strings.HasPrefix(command, "MAIL FROM:"):
			s.mu.Lock()
			s.mailFrom = strings.Trim(strings.TrimSpace(trimmed[len("MAIL FROM:"):]), "<>")
			s.mu.Unlock()
			reply("250 2.1.0 ok")
		case strings.HasPrefix(command, "RCPT TO:"):
			s.mu.Lock()
			s.recipients = append(s.recipients, strings.Trim(strings.TrimSpace(trimmed[len("RCPT TO:"):]), "<>"))
			s.mu.Unlock()
			reply("250 2.1.5 ok")
		case command == "DATA":
			inData = true
			reply("354 end with <CRLF>.<CRLF>")
		case command == "QUIT":
			reply("221 2.0.0 bye")
			return
		case command == "RSET":
			reply("250 2.0.0 ok")
		default:
			reply("250 2.0.0 ok")
		}
	}
}

// port 返回监听端口（用于组装测试配置）。
func (s *fakeSMTP) port(t *testing.T) int {
	t.Helper()
	_, portText, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		t.Fatalf("解析监听地址失败: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("解析监听端口失败: %v", err)
	}
	return port
}

// snapshot 返回服务器记录到的投递细节。
func (s *fakeSMTP) snapshot() (authUser, mailFrom string, recipients []string, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authUser, s.mailFrom, append([]string(nil), s.recipients...), s.data
}

func TestIsEmailRejectsHeaderTricks(t *testing.T) {
	for _, valid := range []string{"ops@example.com", "ops.notify+alert@sub.example.com.cn"} {
		if !IsEmail(valid) {
			t.Fatalf("应接受合法收件人: %s", valid)
		}
	}
	for _, invalid := range []string{
		"", "ops@example.com\r\nBcc: evil@example.com", "ops@example.com,evil@example.com",
		"名字 <ops@example.com>", "ops@example.com 说明", "ops@",
	} {
		if IsEmail(invalid) {
			t.Fatalf("应拒绝非法收件人: %q", invalid)
		}
	}
}

func TestValidateRequiresCompleteConfig(t *testing.T) {
	base := Config{Host: "smtp.example.com", Port: 465, From: "notify@example.com", Recipients: []string{"ops@example.com"}}
	if err := base.Validate(); err != nil {
		t.Fatalf("完整配置应通过校验: %v", err)
	}
	cases := map[string]Config{
		"缺少服务器":  {Port: 465, From: "notify@example.com", Recipients: []string{"ops@example.com"}},
		"端口越界":   {Host: "smtp.example.com", Port: 70000, From: "notify@example.com", Recipients: []string{"ops@example.com"}},
		"缺少发件人":  {Host: "smtp.example.com", Port: 587, Recipients: []string{"ops@example.com"}},
		"发件人格式错": {Host: "smtp.example.com", Port: 587, From: "notify", Recipients: []string{"ops@example.com"}},
		"缺少收件人":  {Host: "smtp.example.com", Port: 587, From: "notify@example.com"},
		"收件人格式错": {Host: "smtp.example.com", Port: 587, From: "notify@example.com", Recipients: []string{"ops"}},
		"加密方式错":  {Host: "smtp.example.com", Port: 587, From: "notify@example.com", Recipients: []string{"ops@example.com"}, Encryption: "quantum"},
	}
	for name, cfg := range cases {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s 应校验失败", name)
		}
	}
}

func TestAddressUsesConfiguredPort(t *testing.T) {
	cfg := Config{Host: "smpt.example.com", Port: 465}
	if got := cfg.Address(); got != "smpt.example.com:465" {
		t.Fatalf("Address = %s", got)
	}
}

func TestBuildMessageEncodesChineseAndFoldsBody(t *testing.T) {
	cfg := Config{From: "抢购提醒 <notify@example.com>", Recipients: []string{"ops@example.com", "backup@example.com"}}
	body := "会话检测发现账号 13800000001 的 Session 已失效。\r\n请重新登录。\r\n" + strings.Repeat("很长的正文行用来确认折行逻辑\n", 40)
	raw := BuildMessage(cfg, "1 个账号 Session 失效", body)

	message, err := mail.ReadMessage(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("生成的邮件无法被解析: %v", err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("主题解码失败: %v", err)
	}
	if subject != "1 个账号 Session 失效" {
		t.Fatalf("主题 = %q", subject)
	}
	if got := message.Header.Get("To"); got != "ops@example.com, backup@example.com" {
		t.Fatalf("To = %q", got)
	}
	if got := message.Header.Get("Content-Transfer-Encoding"); got != "base64" {
		t.Fatalf("Content-Transfer-Encoding = %q", got)
	}
	encoded, err := io.ReadAll(message.Body)
	if err != nil {
		t.Fatalf("读取正文失败: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(encoded), "\r\n", ""))
	if err != nil {
		t.Fatalf("正文不是合法 base64: %v", err)
	}
	// 正文统一按 CRLF 行结束符传输，与原文比较时做同样的规范化。
	expected := strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	if string(decoded) != expected {
		t.Fatal("正文解码结果与原文不一致")
	}
	for _, line := range strings.Split(raw, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("邮件单行超过 SMTP 上限: %d", len(line))
		}
	}
}

func TestSendDeliversToAllRecipients(t *testing.T) {
	server := startFakeSMTP(t, fakeSMTPOptions{})
	cfg := Config{
		Host: "127.0.0.1", Port: server.port(t),
		Username: "notify", Password: "top-secret", From: "抢购提醒 <notify@example.com>",
		Encryption: EncryptionNone, Recipients: []string{"ops@example.com", "backup@example.com"},
	}
	if err := Send(cfg, "会话失效提醒", "账号 13800000001 的 Session 已失效。\n"); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	authUser, mailFrom, recipients, data := server.snapshot()
	if authUser != "notify" {
		t.Fatalf("认证用户 = %q", authUser)
	}
	if mailFrom != "notify@example.com" {
		t.Fatalf("MAIL FROM = %q，应取发件人的纯地址", mailFrom)
	}
	if len(recipients) != 2 || recipients[0] != "ops@example.com" || recipients[1] != "backup@example.com" {
		t.Fatalf("收件人 = %v", recipients)
	}
	if !strings.Contains(data, "Content-Type: text/plain; charset=UTF-8") {
		t.Fatalf("DATA 缺少正文头: %s", data)
	}
}

func TestSendFailsWhenAuthenticationRejected(t *testing.T) {
	server := startFakeSMTP(t, fakeSMTPOptions{rejectAuth: true})
	cfg := Config{
		Host: "127.0.0.1", Port: server.port(t), Username: "notify", Password: "wrong-password",
		From: "notify@example.com", Encryption: EncryptionNone, Recipients: []string{"ops@example.com"},
	}
	err := Send(cfg, "主题", "正文")
	if err == nil || !strings.Contains(err.Error(), "SMTP 认证失败") {
		t.Fatalf("认证被拒绝时应报错，实际: %v", err)
	}
	// 认证失败必须终止投递：不能以未认证身份继续 MAIL FROM/RCPT TO/DATA。
	if _, mailFrom, recipients, data := server.snapshot(); mailFrom != "" || len(recipients) != 0 || data != "" {
		t.Fatalf("认证失败后不应继续投递: from=%q recipients=%v data=%d 字节", mailFrom, recipients, len(data))
	}
}

func TestSendFailsWhenStartTLSUnavailable(t *testing.T) {
	server := startFakeSMTP(t, fakeSMTPOptions{})
	cfg := Config{
		Host: "127.0.0.1", Port: server.port(t), From: "notify@example.com",
		Encryption: EncryptionStartTLS, Recipients: []string{"ops@example.com"},
	}
	err := Send(cfg, "主题", "正文")
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("未提供 STARTTLS 时应报错，实际: %v", err)
	}
}

func TestSendRejectsInvalidConfigWithoutDialing(t *testing.T) {
	cfg := Config{Host: "127.0.0.1", Port: 0, From: "notify@example.com", Recipients: []string{"ops@example.com"}}
	start := time.Now()
	if err := Send(cfg, "主题", "正文"); err == nil {
		t.Fatal("端口非法时应直接报错")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("配置校验应在拨号前完成，耗时 %v", elapsed)
	}
}

// startFakeDNS 启动一个最小 DNS 应答服务器：把任意 A 查询解析为固定地址，用于验证兜底 DNS。
func startFakeDNS(t *testing.T, answer net.IP) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动测试 DNS 失败: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if reply := dnsAReply(buf[:n], answer); reply != nil {
				_, _ = conn.WriteTo(reply, addr)
			}
		}
	}()
	return conn.LocalAddr().String()
}

// dnsAReply 生成一条 A 记录应答：回显请求头部与问题段，附加一条固定地址的 A 记录。
// 请求里的 EDNS0 等附加记录一并丢弃，保证应答格式合法。
func dnsAReply(query []byte, answer net.IP) []byte {
	ip := answer.To4()
	if len(query) < 12 || ip == nil {
		return nil
	}
	end := 12
	for end < len(query) && query[end] != 0 {
		end += int(query[end]) + 1
	}
	end += 5 // 名字结束标记 + QTYPE + QCLASS
	if end > len(query) {
		return nil
	}
	reply := append([]byte(nil), query[:end]...)
	reply[2] |= 0x80
	reply[3] |= 0x80
	reply[6], reply[7] = 0, 1
	reply[10], reply[11] = 0, 0
	reply = append(reply, 0xC0, 0x0C, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x3C, 0x00, 0x04)
	return append(reply, ip...)
}

// TestAlertResolverFallsBackToConfiguredDNS 验证系统解析器失败时回退到兜底 DNS。
func TestAlertResolverFallsBackToConfiguredDNS(t *testing.T) {
	address := startFakeDNS(t, net.IPv4(203, 0, 113, 9))
	resolver := alertResolver{
		primary: &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("系统解析器不可用")
		}},
		servers: []string{address},
	}
	addrs, err := resolver.LookupIPAddr(context.Background(), "smtp.example.com")
	if err != nil {
		t.Fatalf("兜底 DNS 应能解析成功: %v", err)
	}
	if len(addrs) != 1 || addrs[0].IP.String() != "203.0.113.9" {
		t.Fatalf("解析结果 = %v", addrs)
	}
}

// TestAlertResolverPrefersSystemResolver 验证系统解析器可用时不会去问兜底 DNS。
func TestAlertResolverPrefersSystemResolver(t *testing.T) {
	primaryAddress := startFakeDNS(t, net.IPv4(203, 0, 113, 10))
	fallbackAddress := startFakeDNS(t, net.IPv4(203, 0, 113, 9))
	resolver := alertResolver{
		primary: &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, primaryAddress)
		}},
		servers: []string{fallbackAddress},
	}
	addrs, err := resolver.LookupIPAddr(context.Background(), "smtp.example.com")
	if err != nil {
		t.Fatalf("系统解析器可用时应解析成功: %v", err)
	}
	if len(addrs) != 1 || addrs[0].IP.String() != "203.0.113.10" {
		t.Fatalf("应使用系统解析器的结果，实际 %v", addrs)
	}
}

// TestAlertResolverFallsBackWhenSystemResolverReturnsFakeIP 验证系统解析器给出 fake-ip 答案时改问兜底 DNS。
func TestAlertResolverFallsBackWhenSystemResolverReturnsFakeIP(t *testing.T) {
	hijacked := startFakeDNS(t, net.IPv4(198, 18, 0, 4))
	trusted := startFakeDNS(t, net.IPv4(203, 0, 113, 9))
	resolver := alertResolver{
		primary: &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, hijacked)
		}},
		servers: []string{trusted},
	}
	addrs, err := resolver.LookupIPAddr(context.Background(), "smtp.example.com")
	if err != nil {
		t.Fatalf("识别 fake-ip 后应回退兜底 DNS: %v", err)
	}
	if len(addrs) != 1 || addrs[0].IP.String() != "203.0.113.9" {
		t.Fatalf("应使用兜底 DNS 的结果，实际 %v", addrs)
	}
}

// TestDialSMTPImplicitTLSFailsWithoutTLS 覆盖隐式 TLS 分支：对端不是 TLS 服务器时必须报错返回。
func TestDialSMTPImplicitTLSFailsWithoutTLS(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// 立刻关闭连接：TLS 握手必然失败。
			conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	if _, err := dialSMTP(Config{Host: "127.0.0.1", Port: port}, true); err == nil {
		t.Fatal("对端不是 TLS 时应返回错误")
	}
}
