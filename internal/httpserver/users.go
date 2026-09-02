package httpserver

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"gogs-mcp/internal/gogs"
	"gogs-mcp/internal/mcpserver"
	"gogs-mcp/internal/securelog"
)

// userEntry is one resolved Gogs user with the tool server built for it. The
// token itself never enters the cache: the hash keys the entry, and the
// redacting logger holds the only token copy.
type userEntry struct {
	tokenHash string
	user      gogs.User
	server    *mcpserver.Server
	expiresAt time.Time
	element   *list.Element
}

// userCache is an LRU with per-entry TTL over the resolved users. Eviction
// needs no cleanup beyond the log line: a stateless transport holds no
// per-user state besides the cached client and tool server, and in-flight
// requests keep using their entry until they finish.
type userCache struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	entries map[string]*userEntry
	order   *list.List
}

func newUserCache(max int, ttl time.Duration) *userCache {
	return &userCache{
		max:     max,
		ttl:     ttl,
		entries: make(map[string]*userEntry),
		order:   list.New(),
	}
}

func (c *userCache) get(hash string) (*userEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[hash]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		c.remove(entry)
		return nil, false
	}
	entry.expiresAt = time.Now().Add(c.ttl)
	c.order.MoveToFront(entry.element)
	return entry, true
}

// put inserts the entry unless its hash is already present, in which case
// the existing entry wins and the freshly built one is dropped. The returned
// entry is the one that stays cached. The cache owns the expiry clock.
func (c *userCache) put(entry *userEntry) (*userEntry, []int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[entry.tokenHash]; ok {
		return existing, nil
	}
	entry.expiresAt = time.Now().Add(c.ttl)
	entry.element = c.order.PushFront(entry)
	c.entries[entry.tokenHash] = entry

	var evicted []int64
	for len(c.entries) > c.max {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		removed := c.order.Remove(oldest).(*userEntry)
		delete(c.entries, removed.tokenHash)
		evicted = append(evicted, removed.user.ID)
	}
	return entry, evicted
}

func (c *userCache) remove(entry *userEntry) {
	if entry.element != nil {
		c.order.Remove(entry.element)
	}
	delete(c.entries, entry.tokenHash)
}

func (c *userCache) length() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// invalidTokens is a bounded negative cache: a token that Gogs has proven
// invalid is not forwarded to Gogs again until the TTL lapses. Overflow
// resets the whole set, which at worst re-enables a few Gogs probes.
type invalidTokens struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	entries  map[string]time.Time
}

func newInvalidTokens(capacity int, ttl time.Duration) *invalidTokens {
	return &invalidTokens{
		capacity: capacity,
		ttl:      ttl,
		entries:  make(map[string]time.Time),
	}
}

func (i *invalidTokens) has(hash string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	expiresAt, ok := i.entries[hash]
	if !ok {
		return false
	}
	if time.Now().After(expiresAt) {
		delete(i.entries, hash)
		return false
	}
	return true
}

func (i *invalidTokens) add(hash string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.entries) >= i.capacity {
		i.entries = make(map[string]time.Time)
	}
	i.entries[hash] = time.Now().Add(i.ttl)
}

// buildCall is one in-flight token resolution. Concurrent requests with the
// same token wait for its completion instead of probing Gogs again.
type buildCall struct {
	done  chan struct{}
	entry *userEntry
	err   error
}

// resolve turns a token into a cached user entry. The probe request to Gogs
// is the only point where a token is proven; per-hash single-flight stops
// concurrent callers from duplicating it, and the semaphore bounds how many
// probes run at once so that a flood of forged tokens cannot amplify into a
// flood of Gogs requests.
func (s *Server) resolve(ctx context.Context, token, hash string) (*userEntry, error) {
	if entry, ok := s.users.get(hash); ok {
		return entry, nil
	}
	if s.invalid.has(hash) {
		return nil, &gogs.Error{Code: gogs.CodeAuthenticationFailed, Message: "Gogs rejected the supplied credentials."}
	}

	s.buildsMu.Lock()
	if call, ok := s.builds[hash]; ok {
		s.buildsMu.Unlock()
		select {
		case <-call.done:
			return call.entry, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &buildCall{done: make(chan struct{})}
	s.builds[hash] = call
	s.buildsMu.Unlock()
	defer func() {
		s.buildsMu.Lock()
		delete(s.builds, hash)
		s.buildsMu.Unlock()
		close(call.done)
	}()

	call.entry, call.err = s.buildUser(ctx, token, hash)
	return call.entry, call.err
}

// buildUser probes Gogs and assembles the per-user entry. Callers must hold
// the per-hash single-flight slot for this token.
func (s *Server) buildUser(ctx context.Context, token, hash string) (*userEntry, error) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	probe, err := s.newClient(token, "", s.baseLogger)
	if err != nil {
		return nil, err
	}
	user, err := probe.GetAuthenticatedUser(ctx)
	if err != nil {
		classified := gogs.AsError(err)
		switch classified.Code {
		case gogs.CodeAuthenticationFailed, gogs.CodePermissionDenied:
			s.invalid.add(hash)
		}
		return nil, classified
	}

	entry, err := s.buildUserEntry(token, hash, user)
	if err != nil {
		return nil, err
	}
	entry, evicted := s.users.put(entry)
	for _, userID := range evicted {
		s.baseLogger.Debug("Evicted an idle Gogs user.", "user_id", userID)
	}
	s.baseLogger.Info("Authenticated a Gogs user.", "user_id", entry.user.ID, "username", entry.user.Username)
	return entry, nil
}

// buildUserEntry assembles the per-user Gogs client, logger, and tool
// server. The pull request cache lives under cacheRoot/users/<userID> so
// that users never share extracted content, matching the snapshot cache.
func (s *Server) buildUserEntry(token, hash string, user gogs.User) (*userEntry, error) {
	userRoot := filepath.Join(s.options.CacheRoot, "users", strconv.FormatInt(user.ID, 10))
	logger := securelog.NewRedactingLogger(s.options.LogDestination, s.options.LogLevel, token, "token "+token).
		With("user_id", user.ID, "username", user.Username)
	client, err := s.newClient(token, userRoot, logger)
	if err != nil {
		return nil, err
	}
	server := mcpserver.New(client, s.options.Snapshots, logger, s.options.SearchDefaults, s.options.WriteEnabled, mcpserver.WithSchemaCache(s.schemas))
	// The user cache sets the expiry clock on put.
	return &userEntry{
		tokenHash: hash,
		user:      user,
		server:    server,
	}, nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
