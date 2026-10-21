// Package mailer 通过 SMTP 发送纯文本邮件（会话失效提醒等运维通知）。
//
// 只用标准库：支持隐式 TLS（常见 465）、STARTTLS（常见 587）与可信内网的明文中继，
// 以及可选的 PLAIN 认证。主题按 RFC 2047 编码、正文 base64 传输，
// 避免中文与超长行破坏 SMTP 协议。
package mailer

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// 加密方式取值（配置里持久化的字符串）。
const (
	// EncryptionAuto 自动：465 用隐式 TLS，其余端口在服务器支持时用 STARTTLS。
	EncryptionAuto = "auto"
	// EncryptionSSL 隐式 TLS（SMTPS，服务器一连接就是 TLS）。
	EncryptionSSL = "ssl"
	// EncryptionStartTLS 明文连接后升级为 TLS。
	EncryptionStartTLS = "starttls"
	// EncryptionNone 不加密（仅限可信内网中继）。
	EncryptionNone = "none"
)

// 连接与整封邮件的超时上限，避免 SMTP 服务器无响应时长期占用检测循环。
const (
	dialTimeout = 10 * time.Second
	sendTimeout = 30 * time.Second
)

// fallbackDNS 通知邮件的兜底 DNS：系统解析器解析失败时依次尝试。
// 会话失效 / 检测中断提醒必须在本机 DNS 被代理或 TUN 劫持这类故障下仍能送达，
// 否则故障期间根本发不出邮件，只能等环境恢复后补发。
var fallbackDNS = []string{"223.5.5.5", "119.29.29.29"}

// alertResolver 通知邮件的域名解析：先用系统解析器，失败后依次尝试兜底 DNS。
type alertResolver struct {
	// primary 首选解析器；nil 表示系统解析器（测试可注入一个必然失败的解析器）。
	primary *net.Resolver
	// servers 兜底 DNS 服务器列表（host 或 host:port）。
	servers []string
}

// LookupIPAddr 按「系统解析器优先、兜底 DNS 随后」的顺序解析主机名。
// 系统解析器返回 fake-ip 这类被劫持的答案时也改问兜底 DNS：那种答案连不上 SMTP 服务器。
func (r alertResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	primary := r.primary
	if primary == nil {
		primary = net.DefaultResolver
	}
	addrs, err := primary.LookupIPAddr(ctx, host)
	if err == nil && len(addrs) > 0 && !alertAddressHijacked(host, addrs) {
		return addrs, nil
	}
	lookupErr := err
	for _, server := range r.servers {
		address := server
		if _, _, splitErr := net.SplitHostPort(server); splitErr != nil {
			address = net.JoinHostPort(server, "53")
		}
		resolver := &net.Resolver{
			PreferGo: true,
			// 兜底解析走指定的 DNS 服务器，避开被劫持的系统解析器。
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				dialer := net.Dialer{Timeout: dialTimeout}
				return dialer.DialContext(ctx, network, address)
			},
		}
		fallbackAddrs, fallbackErr := resolver.LookupIPAddr(ctx, host)
		if fallbackErr == nil && len(fallbackAddrs) > 0 {
			return fallbackAddrs, nil
		}
		if lookupErr == nil {
			lookupErr = fallbackErr
		}
	}
	if lookupErr == nil {
		lookupErr = fmt.Errorf("解析 %s 失败：系统解析器与兜底 DNS 都不可用", host)
	}
	return nil, lookupErr
}

// alertAddressHijacked 判断系统解析器的答案是否像被代理或 TUN 劫持：
// 主机名解析出 fake-ip 段或回环、未指定、组播地址时，答案都不可用，应改问兜底 DNS。
// 配置里直接写 IP 时不做判断——本地中继场景下回环地址合法。
func alertAddressHijacked(host string, addrs []net.IPAddr) bool {
	if net.ParseIP(strings.TrimSpace(host)) != nil {
		return false
	}
	for _, addr := range addrs {
		if !alertAddressSuspicious(addr.IP) {
			return false
		}
	}
	return true
}

// alertAddressSuspicious 判断单个地址是否明显不是可用的远端地址。
func alertAddressSuspicious(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// 198.18.0.0/15 是 mihomo / sing-box 等代理默认的 fake-ip 段（保留给基准测试，公网不存在真地址）。
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19) {
		return true
	}
	return false
}

// dialSMTP 建立 SMTP 连接：先解析主机（失败时回退兜底 DNS），再按解析结果直连。
// 隐式 TLS 由这里在同一连接上握手完成；SNI 与证书校验仍使用配置里的主机名。
func dialSMTP(cfg Config, implicitTLS bool) (net.Conn, error) {
	// 解析（含兜底 DNS）同样受 dialTimeout 约束，避免 DNS 不通时长时间占住检测循环。
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	addrs, err := alertResolver{servers: fallbackDNS}.LookupIPAddr(ctx, strings.TrimSpace(cfg.Host))
	if err != nil {
		return nil, fmt.Errorf("解析 SMTP 服务器失败: %w", err)
	}
	dialer := &net.Dialer{Timeout: dialTimeout}
	var lastErr error
	for _, addr := range addrs {
		conn, err := dialer.Dial("tcp", net.JoinHostPort(addr.IP.String(), strconv.Itoa(cfg.Port)))
		if err != nil {
			lastErr = err
			continue
		}
		if !implicitTLS {
			return conn, nil
		}
		_ = conn.SetDeadline(time.Now().Add(dialTimeout))
		tlsConn := tls.Client(conn, tlsConfig(cfg))
		if err := tlsConn.Handshake(); err != nil {
			conn.Close()
			lastErr = err
			continue
		}
		// 握手完成后清除拨号超时，后续由整封邮件的发送超时统一控制。
		_ = conn.SetDeadline(time.Time{})
		return tlsConn, nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的 SMTP 服务器地址")
	}
	return nil, fmt.Errorf("连接 SMTP 服务器失败: %w", lastErr)
}

// Config SMTP 发送配置（同时作为数据目录里持久化的结构）。
type Config struct {
	Host       string   `json:"smtpHost"`
	Port       int      `json:"smtpPort"`
	Username   string   `json:"smtpUsername"`
	Password   string   `json:"smtpPassword"`
	From       string   `json:"smtpFrom"`
	Encryption string   `json:"smtpEncryption"`
	Recipients []string `json:"recipients"`
}

// Address 返回 SMTP 服务器地址（host:port）。
func (c Config) Address() string {
	return net.JoinHostPort(strings.TrimSpace(c.Host), strconv.Itoa(c.Port))
}

// ValidateSender 校验发信配置：服务器、端口、加密方式与发件人必须完整且格式合法。
// 不含收件人校验，收件人属于具体业务配置（例如流量告警的收件人）。
func (c Config) ValidateSender() error {
	host := strings.TrimSpace(c.Host)
	if host == "" {
		return errors.New("「邮箱服务器」还没填")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return errors.New("「邮箱服务器」里不能有空格，只填服务器地址")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("「端口」要填 1-65535 之间的数字，现在是 %d", c.Port)
	}
	if _, err := c.encryption(); err != nil {
		return err
	}
	from := strings.TrimSpace(c.From)
	if from == "" {
		return errors.New("「显示的发件人」没填，可以填你的邮箱地址")
	}
	if !IsAddress(c.From) {
		return fmt.Errorf("「显示的发件人」格式不对：%s", from)
	}
	return nil
}

// ValidateRecipients 检查收件人邮箱格式。
func ValidateRecipients(recipients []string) error {
	if len(recipients) == 0 {
		return errors.New("还没填要发的邮箱")
	}
	for _, recipient := range recipients {
		if !IsEmail(recipient) {
			return fmt.Errorf("这个邮箱地址看起来不对：%s", recipient)
		}
	}
	return nil
}

// Validate 校验配置能否直接发信：发信配置与收件人都必须完整且格式合法。
func (c Config) Validate() error {
	if err := c.ValidateSender(); err != nil {
		return err
	}
	return ValidateRecipients(c.Recipients)
}

// IsEmail 判断是否是可直接投递的邮箱地址（不接受「名字 <地址>」形式）。
func IsEmail(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " \t\r\n,;\"<>") {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value
}

// IsAddress 判断是否是可用的邮件地址（允许「名字 <地址>」形式；用于发件人字段）。
func IsAddress(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return false
	}
	_, err := mail.ParseAddress(value)
	return err == nil
}

// encryption 解析加密方式；空值按自动处理，未知值直接报错。
func (c Config) encryption() (string, error) {
	return NormalizeEncryption(c.Encryption)
}

// NormalizeEncryption 归一化加密方式：空值按自动处理，未知值报错。
func NormalizeEncryption(value string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(value))
	switch mode {
	case "":
		return EncryptionAuto, nil
	case EncryptionAuto, EncryptionSSL, EncryptionStartTLS, EncryptionNone:
		return mode, nil
	default:
		return "", fmt.Errorf("不支持的加密方式: %s", value)
	}
}

// Send 向全部收件人投递一封 UTF-8 纯文本邮件。
func Send(cfg Config, subject, body string) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	mode, err := cfg.encryption()
	if err != nil {
		return err
	}
	message := BuildMessage(cfg, subject, body)
	// 自动模式按端口判断：465 是隐式 TLS 的事实标准，其余端口优先 STARTTLS。
	if mode == EncryptionSSL || (mode == EncryptionAuto && cfg.Port == 465) {
		return sendImplicitTLS(cfg, message)
	}
	return sendPlainOrStartTLS(cfg, mode, message)
}

// sendImplicitTLS 用隐式 TLS 连接（服务器一握手就是 TLS）。
func sendImplicitTLS(cfg Config, message string) error {
	conn, err := dialSMTP(cfg, true)
	if err != nil {
		// dialSMTP 已经把错误包装成「连接 SMTP 服务器失败: …」，这里不再重复前缀
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(sendTimeout))
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("初始化 SMTP 会话失败: %w", err)
	}
	return deliver(client, cfg, message)
}

// sendPlainOrStartTLS 明文连接；mode 为 starttls 或 auto 时按服务器能力升级 TLS。
func sendPlainOrStartTLS(cfg Config, mode, message string) error {
	conn, err := dialSMTP(cfg, false)
	if err != nil {
		// 同上：dialSMTP 已带前缀
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(sendTimeout))
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("初始化 SMTP 会话失败: %w", err)
	}
	if mode == EncryptionNone {
		return deliver(client, cfg, message)
	}
	supported, _ := client.Extension("STARTTLS")
	if !supported {
		if mode == EncryptionStartTLS {
			client.Close()
			return errors.New("SMTP 服务器未提供 STARTTLS，请改用 SSL 或明文方式")
		}
		return deliver(client, cfg, message)
	}
	if err := client.StartTLS(tlsConfig(cfg)); err != nil {
		client.Close()
		return fmt.Errorf("STARTTLS 握手失败: %w", err)
	}
	return deliver(client, cfg, message)
}

// tlsConfig 邮件通道的 TLS 参数：校验证书链与主机名，最低 TLS 1.2。
func tlsConfig(cfg Config) *tls.Config {
	return &tls.Config{ServerName: strings.TrimSpace(cfg.Host), MinVersion: tls.VersionTLS12}
}

// deliver 完成认证与投递，返回前关闭 SMTP 客户端（连同底层连接）。
func deliver(client *smtp.Client, cfg Config, message string) error {
	defer client.Close()
	if cfg.Username != "" {
		// 认证失败必须终止投递：否则会以未认证身份继续 MAIL FROM/RCPT TO/DATA。
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			if strings.Contains(err.Error(), "unencrypted connection") {
				return fmt.Errorf("SMTP 认证失败：明文连接不允许发送密码，请改用 SSL 或 STARTTLS: %w", err)
			}
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}
	sender := strings.TrimSpace(cfg.From)
	if address, err := mail.ParseAddress(sender); err == nil {
		sender = address.Address
	}
	if err := client.Mail(sender); err != nil {
		return fmt.Errorf("发送 MAIL FROM 失败: %w", err)
	}
	for _, recipient := range cfg.Recipients {
		if err := client.Rcpt(recipient); err != nil {
			return fmt.Errorf("发送 RCPT TO(%s) 失败: %w", recipient, err)
		}
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("发送 DATA 失败: %w", err)
	}
	if _, err := writer.Write([]byte(message)); err != nil {
		writer.Close()
		return fmt.Errorf("写入邮件正文失败: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("提交邮件正文失败: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("结束 SMTP 会话失败: %w", err)
	}
	return nil
}

// BuildMessage 组装 RFC 5322 邮件内容：主题按 RFC 2047 编码，正文 base64 折行传输。
func BuildMessage(cfg Config, subject, body string) string {
	var builder strings.Builder
	builder.WriteString("From: " + sanitizeHeader(cfg.From) + "\r\n")
	builder.WriteString("To: " + sanitizeHeader(strings.Join(cfg.Recipients, ", ")) + "\r\n")
	builder.WriteString("Subject: " + mime.BEncoding.Encode("UTF-8", sanitizeHeader(subject)) + "\r\n")
	builder.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	builder.WriteString("MIME-Version: 1.0\r\n")
	builder.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	builder.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	builder.WriteString(base64Body(body))
	builder.WriteString("\r\n")
	return builder.String()
}

// sanitizeHeader 去掉头字段里的换行，避免被注入额外头部或伪造正文。
func sanitizeHeader(value string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(value))
}

// base64Body 按 76 列折行编码正文；SMTP 单行长度与点起始行都由 base64 规避。
func base64Body(body string) string {
	normalized := strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(normalized))
	const width = 76
	var builder strings.Builder
	for len(encoded) > width {
		builder.WriteString(encoded[:width])
		builder.WriteString("\r\n")
		encoded = encoded[width:]
	}
	builder.WriteString(encoded)
	return builder.String()
}
