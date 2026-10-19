package solana

import (
	"encoding/json"
	"sync"
	"time"

	"cryptkeeper/internal/store"
)

// MetaCache stores resolved token identities by mint.
type MetaCache interface {
	Get(mint string) (TokenMeta, bool)
	Put(mint string, m TokenMeta)
}

// NewMemoryMetaCache returns a process-lifetime cache that never expires.
func NewMemoryMetaCache() MetaCache { return &memoryMetaCache{m: map[string]TokenMeta{}} }

type memoryMetaCache struct {
	mu sync.RWMutex
	m  map[string]TokenMeta
}

func (c *memoryMetaCache) Get(mint string) (TokenMeta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.m[mint]
	return m, ok
}

func (c *memoryMetaCache) Put(mint string, m TokenMeta) {
	c.mu.Lock()
	c.m[mint] = m
	c.mu.Unlock()
}

// NewSQLiteMetaCache returns a cache backed by the token_meta table. Entries older than ttl are
// treated as misses (and are overwritten by the next Put). A ttl <= 0 means entries never expire.
func NewSQLiteMetaCache(db *store.DB, ttl time.Duration) MetaCache {
	return &sqliteMetaCache{db: db, ttl: ttl}
}

type sqliteMetaCache struct {
	db  *store.DB
	ttl time.Duration
}

func (c *sqliteMetaCache) Get(mint string) (TokenMeta, bool) {
	var raw string
	var fetchedAt int64
	err := c.db.QueryRow(`SELECT json, fetched_at FROM token_meta WHERE mint = ?`, mint).Scan(&raw, &fetchedAt)
	if err != nil {
		return TokenMeta{}, false
	}
	if c.ttl > 0 && time.Since(time.Unix(fetchedAt, 0)) > c.ttl {
		return TokenMeta{}, false
	}
	var m TokenMeta
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return TokenMeta{}, false
	}
	return m, true
}

func (c *sqliteMetaCache) Put(mint string, m TokenMeta) {
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	// Best effort: a failed cache write only costs a repeat lookup.
	_, _ = c.db.Exec(`INSERT INTO token_meta (mint, json, fetched_at) VALUES (?, ?, ?)
		ON CONFLICT(mint) DO UPDATE SET json = excluded.json, fetched_at = excluded.fetched_at`,
		mint, string(raw), time.Now().Unix())
}
