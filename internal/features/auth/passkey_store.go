package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

const passkeySessionTTL = 5 * time.Minute

// PasskeySessionStore 在 begin / finish 之间保存 webauthn.SessionData
//
// 实现说明：
//   - 进程内 map + mutex；dev / 单实例够用
//   - Phase 2：横向扩容时改成 Redis，sessions 通过 session_id key 序列化存储
//   - 一次性消费：Load 命中后立即删除；防止 replay
type PasskeySessionStore struct {
	mu   sync.Mutex
	data map[string]*passkeySessionEntry
}

type passkeySessionEntry struct {
	session   *webauthn.SessionData
	expiresAt time.Time
}

func NewPasskeySessionStore() *PasskeySessionStore {
	return &PasskeySessionStore{data: make(map[string]*passkeySessionEntry)}
}

// Save 写入并返回随机 session_id；客户端在 finish 时回传
func (s *PasskeySessionStore) Save(d *webauthn.SessionData) (string, error) {
	id, err := newSessionID()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.data[id] = &passkeySessionEntry{session: d, expiresAt: time.Now().Add(passkeySessionTTL)}
	s.gcLocked()
	s.mu.Unlock()
	return id, nil
}

// Load 查找并删除 session；过期或未找到返回 (nil, false)
func (s *PasskeySessionStore) Load(id string) (*webauthn.SessionData, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[id]
	if !ok {
		return nil, false
	}
	delete(s.data, id)
	if time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.session, true
}

// gcLocked 顺手清理过期项；调用方持锁
func (s *PasskeySessionStore) gcLocked() {
	if len(s.data) < 64 { // 表小不扫
		return
	}
	now := time.Now()
	for k, v := range s.data {
		if now.After(v.expiresAt) {
			delete(s.data, k)
		}
	}
}

func newSessionID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
