package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type totpState struct {
	LastAcceptedStep int64 `json:"lastAcceptedStep"`
}

// TOTPStateStore 原子记录最近成功时间步，防止动态码在重启前后重复使用。
type TOTPStateStore struct {
	mu   sync.Mutex
	Path string
	Now  func() time.Time
}

// VerifyAndConsume 校验并原子消费一个尚未使用的 TOTP 时间步。
func (s *TOTPStateStore) VerifyAndConsume(secret, code string) bool {
	if s == nil || s.Path == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := loadTOTPState(s.Path)
	if err != nil {
		return false
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	step, ok := VerifyTOTP(secret, code, now, state.LastAcceptedStep)
	if !ok {
		return false
	}
	return saveTOTPState(s.Path, totpState{LastAcceptedStep: step}) == nil
}

// Initialize 保存启用 TOTP 时已经确认并消费的时间步。
func (s *TOTPStateStore) Initialize(step int64) error {
	if s == nil || s.Path == "" || step < 0 {
		return errors.New("TOTP 防重放状态无效")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return saveTOTPState(s.Path, totpState{LastAcceptedStep: step})
}

// Reset 删除已停用 TOTP 的防重放状态。
func (s *TOTPStateStore) Reset() error {
	if s == nil || s.Path == "" {
		return errors.New("TOTP 防重放状态路径无效")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除 TOTP 防重放状态: %w", err)
	}
	return nil
}

func loadTOTPState(path string) (totpState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return totpState{LastAcceptedStep: -1}, nil
	}
	if err != nil {
		return totpState{}, fmt.Errorf("读取 TOTP 状态: %w", err)
	}
	var state totpState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return totpState{}, fmt.Errorf("解析 TOTP 状态: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return totpState{}, errors.New("TOTP 状态只能包含一个 JSON 对象")
		}
		return totpState{}, fmt.Errorf("解析 TOTP 状态尾部内容: %w", err)
	}
	if state.LastAcceptedStep < 0 {
		return totpState{}, errors.New("TOTP 状态时间步无效")
	}
	return state, nil
}

func saveTOTPState(path string, state totpState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("序列化 TOTP 状态: %w", err)
	}
	data = append(data, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("创建 TOTP 状态目录: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".totp-state-*.tmp")
	if err != nil {
		return fmt.Errorf("创建 TOTP 状态临时文件: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("设置 TOTP 状态权限: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("写入 TOTP 状态: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("同步 TOTP 状态: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭 TOTP 状态: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("替换 TOTP 状态: %w", err)
	}
	committed = true
	return nil
}
