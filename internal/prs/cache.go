package prs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	cacheVersion  = 1
	cacheTTL      = 60 * time.Second
	cacheMaxAge   = 7 * 24 * time.Hour
	cacheMaxBytes = 16 << 20
)

// CachedApp wraps App without changing the always-live CLI interface. Configure
// dependencies before use; Load and Detail may then run concurrently.
type CachedApp struct {
	App      *App
	CacheDir string
	Now      func() time.Time
	// ScopeProvider returns an auth identity/token, which is hashed before use.
	// Production resolves credentials on the same GitHubClient used for requests.
	ScopeProvider func(context.Context, GitHub) (string, error)
	writeMu       sync.Mutex
	localSeen     map[string]time.Time // hashed worktree/branch, guarded by writeMu
}

func NewCachedApp(app *App) *CachedApp {
	root, err := os.UserCacheDir()
	if err == nil {
		root = filepath.Join(root, "herdr-prs")
	}
	return &CachedApp{App: app, CacheDir: root, Now: time.Now, ScopeProvider: cacheAuthScope}
}

func cacheAuthScope(ctx context.Context, github GitHub) (string, error) {
	if client, ok := github.(*GitHubClient); ok {
		return client.authToken(ctx)
	}
	// An unknown implementation cannot safely share an authenticated cache.
	return "", nil
}

func cacheHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (a *CachedApp) scope(ctx context.Context, github GitHub) (string, error) {
	provider := a.ScopeProvider
	if provider == nil {
		provider = cacheAuthScope
	}
	identity, err := provider(ctx, github)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Never surface provider errors; they may contain credentials.
		return "", errors.New("GitHub authentication unavailable")
	}
	if identity == "" || a.CacheDir == "" {
		return "", nil
	}
	return cacheHash("github.com\x00" + identity), nil
}

func (a *CachedApp) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

type cacheRecord struct {
	Version  int           `json:"version"`
	Key      string        `json:"key"`
	SavedAt  time.Time     `json:"saved_at"`
	PRs      []PullRequest `json:"prs,omitempty"`
	Detail   *Detail       `json:"detail,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
}

func (r cacheRecord) fresh(now time.Time) bool {
	return !r.SavedAt.IsZero() && !r.SavedAt.After(now) && now.Sub(r.SavedAt) < cacheTTL
}

func (a *CachedApp) cachePath(scope, key string) string {
	return filepath.Join(a.CacheDir, scope, cacheHash(key)+".json")
}

func (a *CachedApp) readCache(scope, key string) (cacheRecord, bool) {
	if scope == "" {
		return cacheRecord{}, false
	}
	path := a.cachePath(scope, key)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > cacheMaxBytes {
		return cacheRecord{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return cacheRecord{}, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, cacheMaxBytes+1))
	if err != nil || len(data) > cacheMaxBytes {
		return cacheRecord{}, false
	}
	var record cacheRecord
	if json.Unmarshal(data, &record) != nil || record.Version != cacheVersion || record.Key != key || record.SavedAt.IsZero() {
		return cacheRecord{}, false
	}
	now := a.now()
	if record.SavedAt.After(now) || now.Sub(record.SavedAt) > cacheMaxAge {
		return cacheRecord{}, false
	}
	if record.Detail != nil && record.Detail.PR.IsLocal() {
		return cacheRecord{}, false
	}
	record.PRs = remotePRs(record.PRs)
	if record.Detail != nil {
		record.Detail.PR.Links = nil
	}
	return record, true
}

// Atomic replacement permits concurrent readers, including other popup processes.
// No tokens, local paths/panes, or failed response bodies are written.
func (a *CachedApp) writeCache(ctx context.Context, scope string, record cacheRecord) error {
	if scope == "" || ctx.Err() != nil {
		return ctx.Err()
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if record.Detail != nil && record.Detail.PR.IsLocal() {
		return errors.New("local work cannot be cached")
	}
	record.PRs = remotePRs(record.PRs)
	if record.Detail != nil {
		detail := *record.Detail
		detail.PR.Links = nil
		record.Detail = &detail
	}
	record.Version = cacheVersion
	data, err := json.Marshal(record)
	if err != nil || len(data) > cacheMaxBytes {
		return errors.New("persistent cache unavailable")
	}
	dir := filepath.Join(a.CacheDir, scope)
	for _, path := range []string{a.CacheDir, dir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
		if err := os.Chmod(path, 0700); err != nil {
			return err
		}
	}
	file, err := os.CreateTemp(dir, ".cache-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Close()
	} else {
		file.Close()
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(file.Name(), a.cachePath(scope, record.Key))
}

func remotePRs(prs []PullRequest) []PullRequest {
	result := make([]PullRequest, 0, len(prs))
	for _, p := range prs {
		if p.IsLocal() {
			continue
		}
		p.Links = nil
		result = append(result, p)
	}
	return result
}
