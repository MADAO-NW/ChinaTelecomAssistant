// Package configs 解析运行时配置与数据目录布局。
//
// 数据目录优先级：-data-dir 参数 > CTM_DATA_DIR 环境变量 > 可执行文件旁 data/。
package configs

import (
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"ctm/models"
)

// Config 数据目录布局。
type Config struct {
	// DataDir 数据根目录（绝对路径）
	DataDir string
}

// Resolve 解析数据目录；override 为 -data-dir 参数值，可为空。
func Resolve(override string) (*Config, error) {
	dir := override
	if dir == "" {
		dir = os.Getenv("CTM_DATA_DIR")
	}
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(filepath.Dir(exe), "data")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	cfg := &Config{DataDir: abs}
	// 同步旧的全局变量，auth/telecom 包仍按它定位会话文件
	DataPath = abs
	return cfg, nil
}

// EnsureDirs 创建数据目录结构；auth 目录权限收紧到 0700。
func (c *Config) EnsureDirs() error {
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{
		{path: c.DataDir, mode: 0o755},
		{path: c.LogDir(), mode: 0o755},
		{path: c.AuthDir(), mode: 0o700},
	} {
		if err := os.MkdirAll(item.path, item.mode); err != nil {
			return err
		}
		if item.path == c.AuthDir() {
			if err := os.Chmod(item.path, item.mode); err != nil {
				return err
			}
		}
	}
	return nil
}

// LogDir 日志目录。
func (c *Config) LogDir() string { return filepath.Join(c.DataDir, "logs") }

// AuthDir 管理端认证数据目录。
func (c *Config) AuthDir() string { return filepath.Join(c.DataDir, "auth") }

// AdminCredentialsFile 管理员凭据文件。
func (c *Config) AdminCredentialsFile() string {
	return filepath.Join(c.AuthDir(), "admin.json")
}

// AdminSessionsFile 管理员正式 Session 持久化文件。
func (c *Config) AdminSessionsFile() string {
	return filepath.Join(c.AuthDir(), "admin-sessions.json")
}

// TOTPStateFile TOTP 防重放状态文件。
func (c *Config) TOTPStateFile() string { return filepath.Join(c.AuthDir(), "totp-state.json") }

// TelecomSessionFile 电信登录态文件（手机号 + token）。
func (c *Config) TelecomSessionFile() string { return filepath.Join(c.DataDir, "session.json") }

// DeviceFile 电信设备号文件。
func (c *Config) DeviceFile() string { return filepath.Join(c.DataDir, "device_id") }

// UpdateStateFile 一键更新的状态快照。
func (c *Config) UpdateStateFile() string { return filepath.Join(c.DataDir, "update-state.json") }

// UpdateRequestFile 一键更新请求文件，由 root 执行器读取后删除。
func (c *Config) UpdateRequestFile() string { return filepath.Join(c.DataDir, "update-request") }

// UpdateResultFile root 执行器写回的执行结果。
func (c *Config) UpdateResultFile() string { return filepath.Join(c.DataDir, "update-result.json") }

// UpdateResultLog 本次更新的完整输出。
func (c *Config) UpdateResultLog() string { return filepath.Join(c.DataDir, "update-result.log") }

// NotifyFile 邮件通知与流量告警配置（含告警记录）。
func (c *Config) NotifyFile() string { return filepath.Join(c.DataDir, "notify.json") }

// ---- 运行时配置（命令行参数解析后写入）----

// Listen 监听地址，默认 0.0.0.0:9035
var Listen string

// Username 电信账号手机号；与 Password 一起用于启动登录、token 过期后的自动重登
var Username string

// Password 电信账号密码；不填则只能用网页上的滑块 + 短信验证码登录
var Password string

// LoginIntervalTime 两次密码登录的最小间隔（秒），防止被上游封号
var LoginIntervalTime int

// TimeOut 访问电信接口的请求超时（秒）
var TimeOut int64

// IntervalsTime 展示接口的防抖间隔（秒）
var IntervalsTime int

// DataPath 数据目录，由 Config.Resolve 同步写入
var DataPath string

// LogLevel 日志等级
var LogLevel string

// LogEncoding 日志输出格式 console 或 json
var LogEncoding string

// Dev 开发模式：开放 /refresh 以及上游原始响应接口
var Dev bool

// PrintVersion 打印版本后退出
var PrintVersion bool

// ClientVersion 密码登录使用的电信客户端版本
var ClientVersion string

// Summary 最近一次成功获取的流量快照
var Summary models.Summary

// Logger 全局日志
var Logger *zap.SugaredLogger
