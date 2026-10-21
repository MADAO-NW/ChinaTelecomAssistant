package admin

import (
	"sync"
	"time"
)

const (
	// loginFailureResetAfter 是无失败后清理计数的固定时间。
	loginFailureResetAfter = 15 * time.Minute
	// loginFailureMaxDelay 是渐进式登录失败延迟上限。
	loginFailureMaxDelay = 30 * time.Second
)

type loginFailure struct {
	count int
	last  time.Time
}

// LoginLimiter 按来源键保存进程内登录失败计数。
type LoginLimiter struct {
	mu       sync.Mutex
	failures map[string]loginFailure
	now      func() time.Time
}

// NewLoginLimiter 创建登录失败限流器。
func NewLoginLimiter() *LoginLimiter {
	return &LoginLimiter{failures: make(map[string]loginFailure), now: time.Now}
}

// Failure 记录一次失败并返回响应前应等待的时长。
func (l *LoginLimiter) Failure(key string) time.Duration {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for currentKey, current := range l.failures {
		if now.Sub(current.last) >= loginFailureResetAfter {
			delete(l.failures, currentKey)
		}
	}
	current := l.failures[key]
	current.count++
	current.last = now
	l.failures[key] = current
	if current.count < 3 {
		return 0
	}
	exponent := current.count - 3
	if exponent >= 5 {
		return loginFailureMaxDelay
	}
	return time.Second << exponent
}

// Success 清除来源键的失败计数。
func (l *LoginLimiter) Success(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}
