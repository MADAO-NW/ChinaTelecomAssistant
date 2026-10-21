package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
)

// Manager 为登录和安全设置提供可原子更新的管理员认证视图。
type Manager struct {
	mu              sync.RWMutex
	credentialsPath string
	credentials     Credentials
	verifier        *Verifier
	totpState       *TOTPStateStore
}

// LoadManager 从正式凭据文件创建管理员认证管理器。
func LoadManager(credentialsPath, totpStatePath string) (*Manager, error) {
	credentials, err := LoadCredentials(credentialsPath)
	if err != nil {
		return nil, err
	}
	verifier, err := NewVerifier(credentials)
	if err != nil {
		return nil, err
	}
	if credentials.TOTP != nil {
		if _, err := os.Stat(totpStatePath); err != nil {
			return nil, fmt.Errorf("检查 TOTP 防重放状态: %w", err)
		}
		if _, err := loadTOTPState(totpStatePath); err != nil {
			return nil, err
		}
	}
	return &Manager{
		credentialsPath: credentialsPath,
		credentials:     credentials,
		verifier:        verifier,
		totpState:       &TOTPStateStore{Path: totpStatePath},
	}, nil
}

// Verify 校验管理员用户名和密码。
func (m *Manager) Verify(username, password string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.verifier.Verify(username, password)
}

// Username 返回管理员用户名。
func (m *Manager) Username() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.verifier.Username()
}

// CredentialFingerprint 返回用于绑定持久化 Session 的管理员凭据指纹。
func (m *Manager) CredentialFingerprint() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	digest := sha256.New()
	_, _ = digest.Write([]byte(m.credentials.Username))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(m.credentials.PasswordHash))
	_, _ = digest.Write([]byte{0})
	if m.credentials.TOTP != nil {
		_, _ = digest.Write([]byte{1})
		_, _ = digest.Write([]byte(m.credentials.TOTP.Secret))
	} else {
		_, _ = digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// TOTPEnabled 返回管理员是否已启用 TOTP。
func (m *Manager) TOTPEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.verifier.TOTPSecret() != ""
}

// VerifyAndConsumeTOTP 校验并持久化消费当前管理员动态码。
func (m *Manager) VerifyAndConsumeTOTP(code string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	secret := m.verifier.TOTPSecret()
	return secret != "" && m.totpState.VerifyAndConsume(secret, code)
}

// ChangeUsername 校验当前密码后原子更新用户名，保留密码和 TOTP 配置。
func (m *Manager) ChangeUsername(currentPassword, username string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.verifier.Verify(m.verifier.Username(), currentPassword) {
		return errors.New("当前密码错误")
	}
	updated := m.credentials
	updated.Username = username
	return m.saveLocked(updated)
}

// ChangePassword 校验当前密码后原子替换密码哈希。
func (m *Manager) ChangePassword(currentPassword, newPassword string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.verifier.Verify(m.verifier.Username(), currentPassword) {
		return errors.New("当前密码错误")
	}
	passwordHash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	updated := m.credentials
	updated.PasswordHash = passwordHash
	return m.saveLocked(updated)
}

// EnableTOTP 保存已经通过动态码确认的 TOTP 密钥。
func (m *Manager) EnableTOTP(secret string, initialStep int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.credentials.TOTP != nil {
		return errors.New("双因素认证已经启用")
	}
	if _, err := decodeTOTPSecret(secret); err != nil {
		return err
	}
	if err := m.totpState.Initialize(initialStep); err != nil {
		return err
	}
	updated := m.credentials
	updated.TOTP = &TOTPConfig{Secret: secret}
	if err := m.saveLocked(updated); err != nil {
		if resetErr := m.totpState.Reset(); resetErr != nil {
			return fmt.Errorf("保存 TOTP 凭据失败且状态清理失败: %v; %w", resetErr, err)
		}
		return err
	}
	return nil
}

// DisableTOTP 删除已经校验通过的管理员 TOTP 配置。
func (m *Manager) DisableTOTP() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.credentials.TOTP == nil {
		return errors.New("双因素认证尚未启用")
	}
	previous := m.credentials
	updated := previous
	updated.TOTP = nil
	if err := m.saveLocked(updated); err != nil {
		return err
	}
	if err := m.totpState.Reset(); err != nil {
		if rollbackErr := m.saveLocked(previous); rollbackErr != nil {
			return fmt.Errorf("清理 TOTP 状态失败且凭据恢复失败: %v; %w", rollbackErr, err)
		}
		return err
	}
	return nil
}

func (m *Manager) saveLocked(credentials Credentials) error {
	verifier, err := NewVerifier(credentials)
	if err != nil {
		return err
	}
	if err := SaveCredentials(m.credentialsPath, credentials); err != nil {
		return err
	}
	m.credentials = credentials
	m.verifier = verifier
	return nil
}
