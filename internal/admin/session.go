package admin

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// AdministratorSessionLifetime 是正式管理员 Session 每次有效访问后的滑动有效期。
	AdministratorSessionLifetime = 30 * 24 * time.Hour
	// pendingSessionIdleTimeout 是双因素认证临时 Session 的空闲有效期。
	pendingSessionIdleTimeout = 30 * time.Minute
	// pendingSessionAbsoluteTimeout 是双因素认证临时 Session 的最长有效期。
	pendingSessionAbsoluteTimeout = 12 * time.Hour
)

type session struct {
	csrfToken     string
	createdAt     time.Time
	lastSeen      time.Time
	authenticated bool
}

type persistedSession struct {
	CSRFToken string    `json:"csrfToken"`
	LastSeen  time.Time `json:"lastSeen"`
}

type persistedSessionState struct {
	CredentialFingerprint string                      `json:"credentialFingerprint"`
	Sessions              map[string]persistedSession `json:"sessions"`
}

// SessionStore 保存管理员 Session；正式 Session 持久化，双因素临时 Session 仅保存在内存。
type SessionStore struct {
	mu                    sync.Mutex
	path                  string
	credentialFingerprint string
	sessions              map[string]session
	now                   func() time.Time
	random                io.Reader
}

// NewSessionStore 创建并加载管理员 Session 仓储。
func NewSessionStore(path, credentialFingerprint string) (*SessionStore, error) {
	return newSessionStore(path, credentialFingerprint, time.Now)
}

func newSessionStore(path, credentialFingerprint string, now func() time.Time) (*SessionStore, error) {
	if path == "" {
		return nil, errors.New("管理员 Session 文件路径不能为空")
	}
	if credentialFingerprint == "" {
		return nil, errors.New("管理员凭据指纹不能为空")
	}
	store := &SessionStore{
		path:                  path,
		credentialFingerprint: credentialFingerprint,
		sessions:              make(map[string]session),
		now:                   now,
		random:                rand.Reader,
	}
	state, exists, err := loadSessionState(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return store, nil
	}
	if state.CredentialFingerprint != credentialFingerprint {
		if err := store.saveLocked(store.sessions, credentialFingerprint); err != nil {
			return nil, fmt.Errorf("清理已失效的管理员 Session: %w", err)
		}
		return store, nil
	}
	currentTime := store.now()
	expired := false
	for sessionID, persisted := range state.Sessions {
		if !validSessionToken(sessionID) || !validSessionToken(persisted.CSRFToken) || persisted.LastSeen.IsZero() {
			return nil, errors.New("管理员 Session 状态内容无效")
		}
		if !currentTime.Before(persisted.LastSeen.Add(AdministratorSessionLifetime)) {
			expired = true
			continue
		}
		store.sessions[sessionID] = session{
			csrfToken:     persisted.CSRFToken,
			createdAt:     persisted.LastSeen,
			lastSeen:      persisted.LastSeen,
			authenticated: true,
		}
	}
	if expired {
		if err := store.saveLocked(store.sessions, credentialFingerprint); err != nil {
			return nil, fmt.Errorf("清理过期管理员 Session: %w", err)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("设置管理员 Session 文件权限: %w", err)
	}
	return store, nil
}

// Create 生成并持久化正式管理员 Session。
func (s *SessionStore) Create() (string, string, error) { return s.create(true) }

// CreatePending 创建只能继续完成 TOTP 的内存临时 Session。
func (s *SessionStore) CreatePending() (string, string, error) { return s.create(false) }

func (s *SessionStore) create(authenticated bool) (string, string, error) {
	csrfToken, err := randomToken(s.random)
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		sessionID, err := randomToken(s.random)
		if err != nil {
			return "", "", err
		}
		if _, exists := s.sessions[sessionID]; exists {
			continue
		}
		now := s.now()
		created := session{
			csrfToken:     csrfToken,
			createdAt:     now,
			lastSeen:      now,
			authenticated: authenticated,
		}
		if !authenticated {
			s.sessions[sessionID] = created
			return sessionID, csrfToken, nil
		}
		next := cloneSessions(s.sessions)
		next[sessionID] = created
		if err := s.saveLocked(next, s.credentialFingerprint); err != nil {
			return "", "", err
		}
		s.sessions = next
		return sessionID, csrfToken, nil
	}
}

// Get 校验正式 Session、刷新最后访问时间并持久化。
func (s *SessionStore) Get(sessionID string) (string, bool, error) {
	if sessionID == "" {
		return "", false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.sessions[sessionID]
	if !exists || !current.authenticated {
		return "", false, nil
	}
	now := s.now()
	if !now.Before(current.lastSeen.Add(AdministratorSessionLifetime)) {
		next := cloneSessions(s.sessions)
		delete(next, sessionID)
		if err := s.saveLocked(next, s.credentialFingerprint); err != nil {
			return "", false, err
		}
		s.sessions = next
		return "", false, nil
	}
	if now.After(current.lastSeen) {
		current.lastSeen = now
	}
	next := cloneSessions(s.sessions)
	next[sessionID] = current
	if err := s.saveLocked(next, s.credentialFingerprint); err != nil {
		return "", false, err
	}
	s.sessions = next
	return current.csrfToken, true, nil
}

// GetPending 校验临时 Session 并刷新最后访问时间。
func (s *SessionStore) GetPending(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.sessions[sessionID]
	if !exists || current.authenticated {
		return "", false
	}
	now := s.now()
	if now.Sub(current.lastSeen) >= pendingSessionIdleTimeout || now.Sub(current.createdAt) >= pendingSessionAbsoluteTimeout {
		delete(s.sessions, sessionID)
		return "", false
	}
	current.lastSeen = now
	s.sessions[sessionID] = current
	return current.csrfToken, true
}

// PromotePending 消费临时 Session，轮换并持久化为新的正式 Session。
func (s *SessionStore) PromotePending(sessionID string) (string, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.sessions[sessionID]
	if !exists || current.authenticated {
		return "", "", false, nil
	}
	now := s.now()
	if now.Sub(current.lastSeen) >= pendingSessionIdleTimeout || now.Sub(current.createdAt) >= pendingSessionAbsoluteTimeout {
		delete(s.sessions, sessionID)
		return "", "", false, nil
	}
	csrfToken, err := randomToken(s.random)
	if err != nil {
		return "", "", false, err
	}
	for {
		newSessionID, err := randomToken(s.random)
		if err != nil {
			return "", "", false, err
		}
		if _, exists := s.sessions[newSessionID]; exists {
			continue
		}
		next := cloneSessions(s.sessions)
		delete(next, sessionID)
		next[newSessionID] = session{
			csrfToken:     csrfToken,
			createdAt:     now,
			lastSeen:      now,
			authenticated: true,
		}
		if err := s.saveLocked(next, s.credentialFingerprint); err != nil {
			return "", "", false, err
		}
		s.sessions = next
		return newSessionID, csrfToken, true, nil
	}
}

// Delete 使指定 Session 立即失效；正式 Session 删除成功后才提交内存状态。
func (s *SessionStore) Delete(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.sessions[sessionID]
	if !exists {
		return nil
	}
	if !current.authenticated {
		delete(s.sessions, sessionID)
		return nil
	}
	next := cloneSessions(s.sessions)
	delete(next, sessionID)
	if err := s.saveLocked(next, s.credentialFingerprint); err != nil {
		return err
	}
	s.sessions = next
	return nil
}

// DeleteAll 使全部正式和临时 Session 失效，并绑定最新管理员凭据指纹。
func (s *SessionStore) DeleteAll(credentialFingerprint string) error {
	if credentialFingerprint == "" {
		return errors.New("管理员凭据指纹不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = make(map[string]session)
	s.credentialFingerprint = credentialFingerprint
	return s.saveLocked(s.sessions, credentialFingerprint)
}

func (s *SessionStore) saveLocked(sessions map[string]session, credentialFingerprint string) error {
	state := persistedSessionState{
		CredentialFingerprint: credentialFingerprint,
		Sessions:              make(map[string]persistedSession),
	}
	for sessionID, current := range sessions {
		if !current.authenticated {
			continue
		}
		state.Sessions[sessionID] = persistedSession{
			CSRFToken: current.csrfToken,
			LastSeen:  current.lastSeen,
		}
	}
	return saveSessionState(s.path, state)
}

func cloneSessions(source map[string]session) map[string]session {
	cloned := make(map[string]session, len(source))
	for sessionID, current := range source {
		cloned[sessionID] = current
	}
	return cloned
}

func loadSessionState(path string) (persistedSessionState, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return persistedSessionState{}, false, nil
	}
	if err != nil {
		return persistedSessionState{}, false, fmt.Errorf("读取管理员 Session 状态: %w", err)
	}
	var state persistedSessionState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return persistedSessionState{}, false, fmt.Errorf("解析管理员 Session 状态: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return persistedSessionState{}, false, errors.New("管理员 Session 状态只能包含一个 JSON 对象")
		}
		return persistedSessionState{}, false, fmt.Errorf("解析管理员 Session 状态尾部内容: %w", err)
	}
	if state.CredentialFingerprint == "" || state.Sessions == nil {
		return persistedSessionState{}, false, errors.New("管理员 Session 状态内容不完整")
	}
	return state, true, nil
}

func saveSessionState(path string, state persistedSessionState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化管理员 Session 状态: %w", err)
	}
	data = append(data, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("创建管理员 Session 目录: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("设置管理员 Session 目录权限: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".admin-sessions-*.tmp")
	if err != nil {
		return fmt.Errorf("创建管理员 Session 临时文件: %w", err)
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
		return fmt.Errorf("设置管理员 Session 状态权限: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("写入管理员 Session 状态: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("同步管理员 Session 状态: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭管理员 Session 状态: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("替换管理员 Session 状态: %w", err)
	}
	committed = true
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("打开管理员 Session 目录: %w", err)
	}
	defer directoryHandle.Close()
	if err := directoryHandle.Sync(); err != nil {
		return fmt.Errorf("同步管理员 Session 目录: %w", err)
	}
	return nil
}

func validSessionToken(value string) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(decoded) == 32
}

func randomToken(source io.Reader) (string, error) {
	data := make([]byte, 32)
	if _, err := io.ReadFull(source, data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
