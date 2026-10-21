package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ctm/configs"
)

// ErrNeedSlider 服务端要求先过滑块验证（resultCode=122001）
var ErrNeedSlider = errors.New("需要滑块验证")

// LoginResult 一次登录成功的结果
type LoginResult struct {
	Token    string `json:"token"`
	Phone    string `json:"phone"`
	Province string `json:"province"`
	City     string `json:"city"`
}

// Session 本地保存的登录态，同时兼容旧版 token.json 与新版的省份/城市字段
type Session struct {
	Phone         string `json:"phone"`
	Token         string `json:"token"`
	LoginLastTime int64  `json:"loginLastTime"`
	Province      string `json:"province,omitempty"`
	City          string `json:"city,omitempty"`
	SavedAt       string `json:"savedAt"`
}

// Manager 串行化登录相关状态：滑块挑战、短信流水号、当前 token
type Manager struct {
	mu    sync.Mutex
	phone string
	token string
	// challenge 登录用滑块挑战；verifySign 是它换来的签名
	challenge  *Challenge
	verifySign string
	smsID      string
}

// sessionPath 会话文件位置
func sessionPath() string {
	return filepath.Join(configs.DataPath, "session.json")
}

// devicePath 设备号文件位置，复用旧版的持久化路径
func devicePath() string {
	return filepath.Join(configs.DataPath, "device_id")
}

// LoadSession 读取本地会话；文件不存在或损坏时返回 nil
func LoadSession() *Session {
	b, err := os.ReadFile(sessionPath())
	if err != nil {
		return nil
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil || strings.TrimSpace(s.Token) == "" {
		return nil
	}
	return &s
}

// SaveSession 写入本地会话，权限 0600
func SaveSession(phone, token, province, city string) error {
	if token == "" {
		return nil
	}
	path := sessionPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(Session{
		Phone:         phone,
		Token:         token,
		LoginLastTime: time.Now().Unix(),
		Province:      province,
		City:          city,
		SavedAt:       time.Now().Format("2006-01-02 15:04:05"),
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// ClearSession 清除本地会话
func ClearSession() error {
	err := os.Remove(sessionPath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DeviceID 读取或生成设备号，与旧版共用同一个文件避免被上游判成新设备
func (m *Manager) DeviceID() (string, error) {
	path := devicePath()
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	id := newUUID()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// New 创建登录管理器，并把本地已保存的会话灌进内存
func New() *Manager {
	m := &Manager{}
	if s := LoadSession(); s != nil {
		m.phone, m.token = s.Phone, s.Token
	}
	return m
}

// State 当前手机号与 token（未脱敏，仅供服务端内部使用）
func (m *Manager) State() (phone, token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.phone, m.token
}

// SetPhone 重置一次登录流程的中间状态
func (m *Manager) SetPhone(phone string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.phone = phone
	m.challenge, m.verifySign, m.smsID = nil, "", ""
}

// SetChallenge 记录本次登录要用的滑块挑战，并清掉旧签名
func (m *Manager) SetChallenge(ch *Challenge) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.challenge, m.verifySign = ch, ""
}

// Challenge 当前登录滑块挑战
func (m *Manager) Challenge() *Challenge {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.challenge
}

// SetVerifySign 记录滑块换来的签名
func (m *Manager) SetVerifySign(sign string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.verifySign = sign
}

// VerifySign 当前滑块签名
func (m *Manager) VerifySign() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.verifySign
}

// ResetSms 已经发过一条短信后强制作废旧滑块与签名，避免复用同一份签名
func (m *Manager) ResetSms() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.smsID != "" {
		m.verifySign, m.challenge = "", nil
	}
}

// MarkSmsSent 记录短信流水号
func (m *Manager) MarkSmsSent(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.smsID = id
}

// StoreToken 登录成功后把 token 写进内存并落盘（调用方保证不在持锁状态调用）
func (m *Manager) StoreToken(res *LoginResult, phone string) error {
	m.mu.Lock()
	m.token, m.phone = res.Token, phone
	m.mu.Unlock()
	return SaveSession(phone, res.Token, res.Province, res.City)
}

// SetToken 手动保存或清除 token（网页上粘贴旧 token 的场景）
func (m *Manager) SetToken(phone, token string) error {
	m.mu.Lock()
	if token == "" {
		m.token = ""
	} else {
		m.token = token
	}
	if strings.TrimSpace(phone) != "" {
		m.phone = phone
	}
	p := m.phone
	m.mu.Unlock()
	if token == "" {
		return ClearSession()
	}
	return SaveSession(p, token, "", "")
}
