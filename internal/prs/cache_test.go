package prs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type cacheGitHub struct {
	*catalogGitHub
	stats                  sync.Mutex
	mineCalls, detailCalls int
	mineHook               func(context.Context) ([]PullRequest, error)
	detailHook             func(context.Context, PullRequest) (Detail, error)
}

func (g *cacheGitHub) Mine(ctx context.Context) ([]PullRequest, error) {
	g.stats.Lock()
	g.mineCalls++
	hook := g.mineHook
	g.stats.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return g.catalogGitHub.Mine(ctx)
}
func (g *cacheGitHub) Detail(ctx context.Context, p PullRequest) (Detail, error) {
	g.stats.Lock()
	g.detailCalls++
	hook := g.detailHook
	g.stats.Unlock()
	if hook != nil {
		return hook(ctx, p)
	}
	return g.catalogGitHub.Detail(ctx, p)
}
func (g *cacheGitHub) counts() (int, int) {
	g.stats.Lock()
	defer g.stats.Unlock()
	return g.mineCalls, g.detailCalls
}
func cacheFixture(t *testing.T, g *cacheGitHub, checkouts ...Checkout) (*CachedApp, *time.Time) {
	t.Helper()
	app := catalogApp(g.catalogGitHub, checkouts...)
	app.GitHubFactory = func() GitHub { return g }
	cached := NewCachedApp(app)
	cached.CacheDir = t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cached.Now = func() time.Time { return now }
	cached.ScopeProvider = func(context.Context, GitHub) (string, error) { return "offline-account", nil }
	return cached, &now
}
func cacheLoad(t *testing.T, a *CachedApp, force bool) []CatalogUpdate {
	t.Helper()
	var updates []CatalogUpdate
	if err := a.Load(context.Background(), force, func(u CatalogUpdate) { updates = append(updates, u) }); err != nil {
		t.Fatal(err)
	}
	if len(updates) == 0 {
		t.Fatal("no catalog emitted")
	}
	return updates
}
func cacheDetail(t *testing.T, a *CachedApp, p PullRequest, force bool) []DetailUpdate {
	t.Helper()
	var updates []DetailUpdate
	if err := a.Detail(context.Background(), p, force, func(u DetailUpdate) { updates = append(updates, u) }); err != nil {
		t.Fatal(err)
	}
	if len(updates) == 0 {
		t.Fatal("no detail emitted")
	}
	return updates
}

func TestCachedLoadWorktreeFirstStagesBeforeMine(t *testing.T) {
	foreign := catalogPR("upstream/r", 1, "fork/r", "topic", "OPEN")
	mine := catalogPR("mine/r", 2, "mine/r", "elsewhere", "OPEN")
	q := (branchQuery{"upstream/r", "topic"}).key()
	started, release := make(chan struct{}), make(chan struct{})
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{branches: map[string][]PullRequest{q: {foreign}}}}
	g.mineHook = func(ctx context.Context) ([]PullRequest, error) {
		close(started)
		select {
		case <-release:
			return []PullRequest{mine}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	a, _ := cacheFixture(t, g, Checkout{Branch: "topic", HeadRepository: "fork/r", Repositories: []string{"upstream/r"}, Links: []Link{{PaneID: "live"}}})
	var discovered bool
	discover := a.App.Discover
	a.App.Discover = func(ctx context.Context, s Snapshot, p, w, popup string) ([]Checkout, []string, error) {
		discovered = true
		return discover(ctx, s, p, w, popup)
	}
	a.ScopeProvider = func(context.Context, GitHub) (string, error) {
		if !discovered {
			t.Error("auth before local discovery")
		}
		return "offline-account", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	updates := make(chan CatalogUpdate, 8)
	done := make(chan error, 1)
	go func() { done <- a.Load(ctx, false, func(u CatalogUpdate) { updates <- u }) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("Mine never started")
	}
	select {
	case u := <-updates:
		if u.Cached || len(u.Catalog.PRs) != 1 || !u.Catalog.PRs[0].IsLocal() || len(u.Catalog.PRs[0].Links) != 1 {
			t.Fatalf("local stage = %+v", u)
		}
	default:
		t.Fatal("missing immediate local stage")
	}
	select {
	case u := <-updates:
		if u.Cached || !reflect.DeepEqual(catalogKeys(u.Catalog), []string{"upstream/r#1"}) || len(u.Catalog.PRs[0].Links) != 1 {
			t.Fatalf("branch stage = %+v", u)
		}
	default:
		t.Fatal("Mine began without emitting linked rows")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	last := <-updates
	if !reflect.DeepEqual(catalogKeys(last.Catalog), []string{"mine/r#2", "upstream/r#1"}) {
		t.Fatal(last)
	}
}

func TestCachedLoadTTLRebindNewBranchesAndNoForeignLeak(t *testing.T) {
	q := (branchQuery{"up/r", "topic"}).key()
	foreign := catalogPR("up/r", 1, "fork/r", "topic", "MERGED")
	mine := catalogPR("mine/r", 2, "mine/r", "elsewhere", "OPEN")
	mine.CreatedAt, mine.GroupTiming = "2025-12-01T00:00:00Z", &GroupTiming{Group: mine.Group(), Since: "2025-12-02T00:00:00Z"}
	foreign.CreatedAt, foreign.GroupTiming = mine.CreatedAt, &GroupTiming{Group: foreign.Group(), Since: "2025-12-03T00:00:00Z"}
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{mine: []PullRequest{mine}, branches: map[string][]PullRequest{q: {foreign}}}}
	checkout := Checkout{Branch: "topic", HeadRepository: "fork/r", Repositories: []string{"up/r"}, Links: []Link{{PaneID: "old", Path: "private-local-path"}}}
	a, now := cacheFixture(t, g, checkout)
	cacheLoad(t, a, false)
	// A new wrapper proves persistence, not merely popup memory.
	b := NewCachedApp(a.App)
	b.CacheDir, b.Now, b.ScopeProvider = a.CacheDir, a.Now, a.ScopeProvider
	discoveries := 0
	checkout.Links = []Link{{PaneID: "new"}}
	b.App.Discover = func(context.Context, Snapshot, string, string, string) ([]Checkout, []string, error) {
		discoveries++
		return []Checkout{checkout}, nil, nil
	}
	*now = now.Add(59 * time.Second)
	updates := cacheLoad(t, b, false)
	if len(updates) != 2 || updates[0].Cached || len(updates[0].Catalog.PRs) != 1 || !updates[0].Catalog.PRs[0].IsLocal() || updates[1].Cached || discoveries != 1 {
		t.Fatal(updates, discoveries)
	}
	if len(updates[1].Catalog.PRs) != 2 {
		t.Fatal("warm cache lost rows", updates[1])
	}
	for _, p := range updates[1].Catalog.PRs {
		want := mine
		if p.Key() == foreign.Key() {
			want = foreign
			want.Links = checkout.Links
		}
		if !reflect.DeepEqual(p, want) {
			t.Fatal("persisted snapshot/timing or rebound links changed", p, want)
		}
	}
	if calls, _ := g.counts(); calls != 1 || g.calls[q] != 1 {
		t.Fatal("fresh cache used network", calls, g.calls)
	}
	// New open branch is queried even though the authored search is still fresh.
	second := Checkout{Branch: "new-topic", HeadRepository: "fork/r", Repositories: []string{"up/r"}, Links: []Link{{PaneID: "second"}}}
	newQ := (branchQuery{"up/r", "new-topic"}).key()
	g.branches[newQ] = []PullRequest{catalogPR("up/r", 3, "fork/r", "new-topic", "OPEN")}
	b.App.Discover = func(context.Context, Snapshot, string, string, string) ([]Checkout, []string, error) {
		return []Checkout{checkout, second}, nil, nil
	}
	updates = cacheLoad(t, b, false)
	if !reflect.DeepEqual(catalogKeys(updates[len(updates)-1].Catalog), []string{"mine/r#2", "up/r#1", "up/r#3"}) {
		t.Fatal(updates)
	}
	if calls, _ := g.counts(); calls != 1 || g.calls[q] != 1 || g.calls[newQ] != 1 {
		t.Fatal(calls, g.calls)
	}
	// Removing all checkouts drops foreign/completed rows but keeps authored rows.
	b.App.Discover = func(context.Context, Snapshot, string, string, string) ([]Checkout, []string, error) {
		return nil, nil, nil
	}
	updates = cacheLoad(t, b, false)
	if !reflect.DeepEqual(catalogKeys(updates[0].Catalog), []string{"mine/r#2"}) {
		t.Fatal(updates)
	}
	filepath.WalkDir(a.CacheDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if !d.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "private-local-path") || strings.Contains(string(data), "offline-account") || strings.Contains(string(data), "old") {
				t.Fatalf("local links or identity persisted: %s", path)
			}
		}
		return nil
	})
}

func TestCachedLoadForceFailureRetainsSourcesNotFreshAndClearsClosed(t *testing.T) {
	q := (branchQuery{"up/r", "topic"}).key()
	foreign := catalogPR("up/r", 1, "fork/r", "topic", "OPEN")
	mine := catalogPR("mine/r", 2, "mine/r", "elsewhere", "OPEN")
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{mine: []PullRequest{mine}, branches: map[string][]PullRequest{q: {foreign}}}}
	a, now := cacheFixture(t, g, Checkout{Branch: "topic", HeadRepository: "fork/r", Repositories: []string{"up/r"}, Links: []Link{{PaneID: "live"}}})
	cacheLoad(t, a, false)
	failure := errors.New("offline")
	g.mineErr = failure
	g.branchErrors = map[string]error{q: failure}
	*now = now.Add(30 * time.Second)
	var updates []CatalogUpdate
	err := a.Load(context.Background(), true, func(u CatalogUpdate) { updates = append(updates, u) })
	if !errors.Is(err, failure) || len(updates) < 3 {
		t.Fatal(err, updates)
	}
	// Forced refresh retains known PR rows instead of flashing provisional
	// local-only rows; every publication here follows account verification.
	for _, u := range updates {
		if !u.Cached || !reflect.DeepEqual(catalogKeys(u.Catalog), []string{"mine/r#2", "up/r#1"}) {
			t.Fatal(u)
		}
	}
	if !catalogWarning(updates[len(updates)-1].Catalog.Warnings, "offline") {
		t.Fatal(updates)
	}
	// The failed branch remains stale while a successful Mine refresh removes closed authored rows.
	*now = now.Add(31 * time.Second)
	g.mine, g.mineErr = nil, nil
	updates = nil
	if err := a.Load(context.Background(), false, func(u CatalogUpdate) { updates = append(updates, u) }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if mineCalls, _ := g.counts(); mineCalls != 3 || g.calls[q] != 3 {
		t.Fatal("failed refresh advanced source freshness", mineCalls, g.calls)
	}
	last := updates[len(updates)-1]
	if !last.Cached || !reflect.DeepEqual(catalogKeys(last.Catalog), []string{"up/r#1"}) {
		t.Fatal(last)
	}
	// A successful empty branch response clears its old rows rather than unioning them forever.
	g.branchErrors, g.branches[q] = nil, nil
	updates = cacheLoad(t, a, false)
	last = updates[len(updates)-1]
	if last.Cached || len(last.Catalog.PRs) != 1 || !last.Catalog.PRs[0].IsLocal() || len(last.Catalog.PRs[0].Links) != 1 || last.Catalog.PRs[0].Links[0].PaneID != "live" {
		t.Fatal(last)
	}
}

func TestCachedDetailTTLInvalidationLinksAndFailure(t *testing.T) {
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
	a, now := cacheFixture(t, g)
	p := catalogPR("up/r", 1, "fork/r", "topic", "OPEN")
	p.HeadOID, p.UpdatedAt = "head1", "updated1"
	p.CreatedAt, p.GroupTiming = "2025-12-01T00:00:00Z", &GroupTiming{Group: p.Group(), Since: "2025-12-02T00:00:00Z"}
	p.Links = []Link{{PaneID: "old", Path: "/private-detail-path"}}
	initial := cacheDetail(t, a, p, false)
	if len(initial) != 1 || initial[0].Cached || initial[0].FromCache {
		t.Fatal("live detail marked cached", initial)
	}
	scope, err := a.scope(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.cachePath(scope, detailCacheKey(p)))
	if err != nil || strings.Contains(string(data), "/private-detail-path") || strings.Contains(string(data), `"pane_id":"old"`) {
		t.Fatal("detail persisted computed navigation", err)
	}
	p.Links = []Link{{PaneID: "live"}}
	// Reopen to verify timing and raw detail persisted, not just popup memory.
	b := NewCachedApp(a.App)
	b.CacheDir, b.Now, b.ScopeProvider = a.CacheDir, a.Now, a.ScopeProvider
	updates := cacheDetail(t, b, p, false)
	if len(updates) != 1 || updates[0].Cached || !updates[0].FromCache || !reflect.DeepEqual(updates[0].Detail.PR, p) {
		t.Fatal(updates)
	}
	if _, calls := g.counts(); calls != 1 {
		t.Fatal(calls)
	}
	// Both metadata fields independently invalidate the otherwise fresh cache.
	for _, field := range []string{"head", "updated"} {
		if field == "head" {
			p.HeadOID = "head2"
		} else {
			p.UpdatedAt = "updated2"
		}
		updates = cacheDetail(t, a, p, false)
		if len(updates) != 2 || !updates[0].Cached || !updates[0].FromCache || updates[1].Cached || updates[1].FromCache {
			t.Fatal(field, updates)
		}
	}
	failure := errors.New("offline detail")
	g.detailHook = func(context.Context, PullRequest) (Detail, error) { return Detail{}, failure }
	updates = nil
	if err := a.Detail(context.Background(), p, true, func(u DetailUpdate) { updates = append(updates, u) }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if len(updates) != 2 || !updates[0].Cached || !updates[0].FromCache || !updates[1].Cached || !updates[1].FromCache || catalogWarning(updates[1].Detail.Warnings, "cached") {
		t.Fatal(updates)
	}
	*now = now.Add(time.Minute)
	g.detailHook = nil
	updates = cacheDetail(t, a, p, false)
	if len(updates) != 2 || !updates[0].Cached || !updates[0].FromCache || updates[1].Cached || updates[1].FromCache {
		t.Fatal(updates)
	}
	other := p
	other.Number++
	updates = cacheDetail(t, a, other, false)
	if len(updates) != 1 || updates[0].Detail.PR.Number != other.Number {
		t.Fatal("wrong PR cache", updates)
	}
	other.Repository = "other/r"
	updates = cacheDetail(t, a, other, false)
	if len(updates) != 1 || updates[0].Detail.PR.Repository != other.Repository {
		t.Fatal("wrong repo cache", updates)
	}
}

func TestCacheScopeIsolationAndAuthFailure(t *testing.T) {
	p := catalogPR("private/r", 1, "private/r", "topic", "OPEN")
	p.CreatedAt, p.GroupTiming = "2025-12-01T00:00:00Z", &GroupTiming{Group: p.Group(), Since: "2025-12-02T00:00:00Z"}
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{mine: []PullRequest{p}}}
	g.detailHook = func(context.Context, PullRequest) (Detail, error) { return Detail{PR: p, Body: "private prose"}, nil }
	a, _ := cacheFixture(t, g)
	cacheLoad(t, a, false)
	cacheDetail(t, a, p, false)
	a.ScopeProvider = func(context.Context, GitHub) (string, error) { return "other-token", nil }
	g.mine = nil
	g.detailHook = func(context.Context, PullRequest) (Detail, error) {
		return Detail{PR: p, Body: "other account prose"}, nil
	}
	updates := cacheLoad(t, a, false)
	if len(updates) != 1 || len(updates[0].Catalog.PRs) != 0 {
		t.Fatal("cross-account rows/timing", updates)
	}
	details := cacheDetail(t, a, p, false)
	if len(details) != 1 || details[0].FromCache || details[0].Detail.Body != "other account prose" {
		t.Fatal("cross-account detail", details)
	}
	a.ScopeProvider = func(context.Context, GitHub) (string, error) { return "", errors.New("secret-token") }
	called := false
	err := a.Load(context.Background(), false, func(CatalogUpdate) { called = true })
	if err == nil || called || strings.Contains(err.Error(), "secret") {
		t.Fatal("unsafe auth failure", err, called)
	}
	err = a.Detail(context.Background(), p, false, func(DetailUpdate) { called = true })
	if err == nil || called || strings.Contains(err.Error(), "secret") {
		t.Fatal("unsafe detail auth failure", err, called)
	}
	if mineCalls, detailCalls := g.counts(); mineCalls != 2 || detailCalls != 2 {
		t.Fatal("network after auth failure", mineCalls, detailCalls)
	}
	// Production auth/cache seam: one token resolution scopes the same HTTP
	// adapter, then a warm load verifies auth without fetching remote sources.
	client, requests := ghTestQueued(t, ghTestObject{"viewer": ghTestObject{"login": "me"}}, ghTestSearch([]any{ghTestNode(1)}, false, nil, 1))
	resolutions := 0
	client.tokenSource = func(context.Context) (string, error) { resolutions++; return "offline-secret", nil }
	a.App.GitHubFactory = func() GitHub { return client }
	a.ScopeProvider = nil
	first := cacheLoad(t, a, false)
	second := cacheLoad(t, a, false)
	if len(second) != 1 || second[0].Cached || !reflect.DeepEqual(second[0].Catalog.PRs, first[len(first)-1].Catalog.PRs) || !reflect.DeepEqual(catalogKeys(second[0].Catalog), []string{"o/r#1"}) || resolutions != 1 || len(*requests) != 2 {
		t.Fatal("warm authenticated cache fetched sources/lost scope", second, resolutions, len(*requests))
	}
}

func TestCacheValidationBoundsPermissionsAndAtomicReplace(t *testing.T) {
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
	a, now := cacheFixture(t, g)
	scope, _ := a.scope(context.Background(), g)
	record := cacheRecord{Key: "mine", SavedAt: *now, PRs: []PullRequest{catalogPR("up/r", 1, "up/r", "topic", "OPEN")}}
	if err := a.writeCache(context.Background(), scope, record); err != nil {
		t.Fatal(err)
	}
	path := a.cachePath(scope, "mine")
	for _, name := range []string{a.CacheDir, filepath.Dir(path), path} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0700)
		if !info.IsDir() {
			want = 0600
		}
		if info.Mode().Perm() != want {
			t.Fatal(name, info.Mode())
		}
	}
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	oversized := cacheRecord{Key: "mine", SavedAt: *now, Detail: &Detail{PR: record.PRs[0], Body: strings.Repeat("x", cacheMaxBytes)}}
	if err := a.writeCache(context.Background(), scope, oversized); err == nil {
		t.Fatal("accepted oversized write")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != string(valid) {
		t.Fatal("rejected write damaged existing snapshot", err)
	}
	cases := map[string][]byte{
		"corrupt":   []byte("{"),
		"version":   []byte(strings.Replace(string(valid), `"version":1`, `"version":999`, 1)),
		"future":    []byte(strings.Replace(string(valid), now.Format(time.RFC3339), now.Add(time.Second).Format(time.RFC3339), 1)),
		"expired":   []byte(strings.Replace(string(valid), now.Format(time.RFC3339), now.Add(-cacheMaxAge-time.Second).Format(time.RFC3339), 1)),
		"wrong key": []byte(strings.Replace(string(valid), `"key":"mine"`, `"key":"other"`, 1)),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, ok := a.readCache(scope, "mine"); ok {
				t.Fatal("accepted invalid record")
			}
		})
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(cacheMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, ok := a.readCache(scope, "mine"); ok {
		t.Fatal("accepted oversized cache")
	}
	// Refuse linked records; atomic replacement must not write through the link.
	target := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(target, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.readCache(scope, "mine"); ok {
		t.Fatal("trusted symlinked cache")
	}
	record.PRs[0].Title = "replacement"
	if err := a.writeCache(context.Background(), scope, record); err != nil {
		t.Fatal(err)
	}
	if stored, ok := a.readCache(scope, "mine"); !ok || len(stored.PRs) != 1 || stored.PRs[0].Title != "replacement" {
		t.Fatal("atomic replacement unreadable", stored)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != string(valid) {
		t.Fatal("atomic replacement followed symlink", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
}

func TestCachedCancellationDropsLateUpdatesAndWrites(t *testing.T) {
	for _, operation := range []string{"load", "detail"} {
		t.Run(operation, func(t *testing.T) {
			g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
			a, _ := cacheFixture(t, g, Checkout{Path: "/local", Branch: "topic", HeadRepository: "up/r", Repositories: []string{"up/r"}})
			started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			p := catalogPR("up/r", 1, "up/r", "topic", "OPEN")
			g.branch = func(context.Context, string, string) ([]PullRequest, error) {
				close(started)
				<-release
				close(returned)
				return []PullRequest{p}, nil
			}
			g.detailHook = func(context.Context, PullRequest) (Detail, error) {
				close(started)
				<-release
				close(returned)
				return Detail{PR: p}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			var mu sync.Mutex
			updates := 0
			count := func() { mu.Lock(); updates++; mu.Unlock() }
			go func() {
				if operation == "load" {
					done <- a.Load(ctx, false, func(CatalogUpdate) { count() })
				} else {
					done <- a.Detail(ctx, p, false, func(DetailUpdate) { count() })
				}
			}()
			<-started
			cancel()
			close(release)
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			<-returned
			mu.Lock()
			wantUpdates := 0
			if operation == "load" {
				wantUpdates = 1 // Local discovery stage, before cancellation.
			}
			if updates != wantUpdates {
				t.Fatal("late updates", updates)
			}
			mu.Unlock()
			entries, err := os.ReadDir(a.CacheDir)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "load" {
				// Local first-seen was persisted before auth/network began.
				if len(entries) != 1 || entries[0].Name() != localObservationDir {
					t.Fatal("late remote write", entries)
				}
			} else if len(entries) != 0 {
				t.Fatal("late write", entries)
			}
		})
	}
}

func TestCachedConcurrentLoadAndDetail(t *testing.T) {
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{mine: []PullRequest{catalogPR("up/r", 1, "up/r", "topic", "OPEN")}}}
	a, _ := cacheFixture(t, g)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if err := a.Load(context.Background(), true, func(CatalogUpdate) {}); err != nil {
					t.Error(err)
				}
			} else {
				p := catalogPR("up/r", i, "up/r", fmt.Sprint(i), "OPEN")
				if err := a.Detail(context.Background(), p, true, func(DetailUpdate) {}); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	updates := cacheLoad(t, a, false)
	if len(updates) != 1 || updates[0].Cached || !reflect.DeepEqual(catalogKeys(updates[0].Catalog), []string{"up/r#1"}) {
		t.Fatal("concurrent catalog writes unreadable", updates)
	}
	for i := 1; i < 8; i += 2 {
		p := catalogPR("up/r", i, "up/r", fmt.Sprint(i), "OPEN")
		details := cacheDetail(t, a, p, false)
		if len(details) != 1 || !details[0].FromCache || details[0].Cached || !reflect.DeepEqual(details[0].Detail, Detail{PR: p, Body: "offline detail"}) {
			t.Fatal("concurrent detail writes unreadable", details)
		}
	}
	if mine, detail := g.counts(); mine != 4 || detail != 4 {
		t.Fatal("concurrent persistence lost warm cache", mine, detail)
	}
}
