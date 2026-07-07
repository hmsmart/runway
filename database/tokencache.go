package database

import (
	"crypto/rand"
	"encoding/base32"
	"sync"
	"time"
)

type tokenCache struct {
	mu     sync.RWMutex
	tokens map[string]time.Time
}

func newTokenCache() *tokenCache {
	newCache := &tokenCache{
		tokens: make(map[string]time.Time),
	}
	go func() {
		for range time.Tick(3 * time.Hour) {
			newCache.mu.Lock()
			now := time.Now()
			for k, v := range newCache.tokens {
				if now.After(v) {
					delete(newCache.tokens, k)
				}
			}
			newCache.mu.Unlock()
		}
	}()
	return newCache
}

func (tc *tokenCache) ConsumeToken(token string) bool {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	exp, exists := tc.tokens[token]
	if !exists {
		return false
	}
	if time.Now().Before(exp) {
		delete(tc.tokens, token)
		return true
	} else {
		delete(tc.tokens, token)
		return false
	}
}

func (tc *tokenCache) GenerateToken() string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	token := randomToken(8)
	exp := time.Now().Add(30 * time.Minute)
	tc.tokens[token] = exp
	return token
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base32.StdEncoding.EncodeToString(b)
}
