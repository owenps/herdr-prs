package prs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLocalGitDiscoveryNoRemoteAndStableCheckoutID(t *testing.T) {
	root := herdrGitRepo(t)
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	s := Snapshot{Workspaces: []Workspace{herdrWorkspace("ws", root)}, Tabs: []Tab{{ID: "tab", Label: "Agent task"}}, Panes: []Pane{
		{ID: "origin", WorkspaceID: "ws", TabID: "tab", CWD: nested},
		{ID: "agent", WorkspaceID: "ws", CWD: alias, Agent: "helper"},
		{ID: "missing", WorkspaceID: "ws"},
	}}
	d := Discoverer{}
	found, warnings, err := d.Discover(context.Background(), s, "origin", "ws", "")
	if err != nil || len(warnings) != 0 || len(found) != 1 {
		t.Fatal(found, warnings, err)
	}
	c := found[0]
	if c.LocalID != root || c.Path != root || c.LocalBranch != "local" || c.HeadRepository != "" || len(checkoutQueries(found)) != 0 || len(c.Links) != 4 {
		t.Fatal(c)
	}
	p := assembleCatalog(nil, time.Time{}, found, nil, nil, nil).PRs[0]
	if !p.IsLocal() || p.Key() != "local:"+root || p.Title != "Agent task" || p.Status() != "" || p.Group() != GroupInProgress || p.Repository != "" || p.HeadRepository != "" || p.Number != 0 || p.URL != "" {
		t.Fatal(p)
	}
	for _, link := range c.Links {
		if link.PaneID == "missing" {
			if link.Path != "" { // Missing pane CWD must remain navigable.
				t.Fatal(link)
			}
		} else if link.Path != root || link.LocalBranch != "local" {
			t.Fatal(link)
		}
	}
	herdrGit(t, root, "checkout", "-b", "renamed")
	found, _, err = d.Discover(context.Background(), s, "origin", "ws", "")
	if err != nil || found[0].LocalID != c.LocalID || found[0].LocalBranch != "renamed" {
		t.Fatal(found, err)
	}
}

func TestLocalDiscoveryAmbiguousPushDestinations(t *testing.T) {
	for _, urls := range [][]string{
		{"https://github.com/fork/r.git", "https://github.com/other/r.git"},
		{"https://github.com/fork/r.git", "https://gitlab.com/fork/r.git"},
		{"https://gitlab.com/fork/r.git", "https://github.com/fork/r.git"},
	} {
		root := herdrGitRepo(t)
		herdrGit(t, root, "remote", "add", "origin", "https://github.com/fork/r.git")
		for _, url := range urls {
			herdrGit(t, root, "config", "--local", "--add", "remote.origin.pushurl", url)
		}
		found, warnings, err := (Discoverer{}).Discover(context.Background(), Snapshot{Panes: []Pane{{ID: "p", CWD: root}}}, "p", "", "")
		if err != nil || len(found) != 1 || found[0].HeadRepository != "" || len(found[0].Repositories) != 0 || len(checkoutQueries(found)) != 0 || !catalogWarning(warnings, "ambiguous head repository") {
			t.Fatal(found, warnings, err)
		}
	}
}

func TestNonGitSpacesDoNotCreateWorkRowsOrHideAuthoredPRs(t *testing.T) {
	cwd := canonicalPath(t.TempDir())
	s := Snapshot{Workspaces: []Workspace{{ID: "one", Label: "One"}, {ID: "empty", Label: "Empty"}}, Panes: []Pane{
		{ID: "a", WorkspaceID: "one", CWD: cwd},
		{ID: "b", WorkspaceID: "one", CWD: cwd},
		{ID: "unknown-cwd", WorkspaceID: "one"},
		{ID: "independent", CWD: cwd},
	}}
	found, warnings, err := (Discoverer{}).Discover(context.Background(), s, "a", "one", "")
	if err != nil || len(warnings) != 0 || len(found) != 0 {
		t.Fatal(found, warnings, err)
	}
	p := catalogPR("o/r", 1, "o/r", "main", "OPEN")
	catalog := assembleCatalog([]PullRequest{p}, time.Time{}, found, nil, nil, nil)
	if len(catalog.PRs) != 1 || catalog.PRs[0].Key() != p.Key() || len(catalog.PRs[0].Links) != 0 {
		t.Fatal("non-Git exclusion affected authored PR visibility", catalog)
	}
}

func TestInProgressRowsExcludeKnownDefaultsAndDetachedCheckouts(t *testing.T) {
	for _, tc := range []struct {
		name, local, mapped, defaultBranch string
		want                               bool
	}{
		{"primary feature checkout", "feature", "feature", "develop", true},
		{"custom default", "develop", "develop", "develop", false},
		{"default in linked worktree", "release/stable", "release/stable", "release/stable", false},
		{"main is a feature here", "main", "main", "develop", true},
		{"master is a feature here", "master", "master", "develop", true},
		{"local name is authoritative", "develop", "remote-alias", "develop", false},
		{"unknown default not guessed", "main", "main", "", true},
		{"no remote feature", "feature", "feature", "", true},
		{"detached", "", "", "develop", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Checkout{Path: "/checkout", Branch: tc.mapped, LocalBranch: tc.local, DefaultBranch: tc.defaultBranch, Links: []Link{{PaneID: "p", Path: "/checkout"}}}
			catalog := assembleCatalog(nil, time.Time{}, []Checkout{c}, nil, nil, nil)
			if (len(catalog.PRs) == 1) != tc.want {
				t.Fatal(catalog)
			}
			if tc.want && (!catalog.PRs[0].IsLocal() || catalog.PRs[0].Status() != "" || catalog.PRs[0].Group() != GroupInProgress) {
				t.Fatal(catalog)
			}
		})
	}
}

func TestLocalCatalogReplacedOnlyByMatchingPR(t *testing.T) {
	c := Checkout{Path: "/checkout", LocalBranch: "topic", Branch: "topic", HeadRepository: "fork/r", Repositories: []string{"up/r"}, Title: "Task label", Links: []Link{{WorkspaceID: "ws", PaneID: "p", Path: "/checkout", LocalBranch: "topic", Label: "Workspace label"}}, LocalTiming: &GroupTiming{Group: GroupInProgress, Since: timingCreated, Approximate: true}}
	key := (branchQuery{"up/r", "topic"}).key()
	foreign := catalogPR("up/r", 1, "other/r", "topic", "OPEN")
	matching := catalogPR("up/r", 2, "fork/r", "topic", "OPEN")
	matching.Title = "Actual PR title"
	g := &catalogGitHub{branches: map[string][]PullRequest{key: {foreign}}}
	app := catalogApp(g, c)
	catalog, err := app.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.PRs) != 1 {
		t.Fatal(catalog)
	}
	p := catalog.PRs[0]
	if !p.IsLocal() || p.Key() != "local:/checkout" || p.Title != "Task label" || p.Local.Repository != "fork/r" || p.Repository != "" || p.HeadRepository != "" || p.Status() != "" || !reflect.DeepEqual(p.GroupTiming, c.LocalTiming) {
		t.Fatal(p)
	}
	for _, state := range []string{"OPEN", "MERGED", "CLOSED"} {
		matching.State, matching.Draft = state, true
		group := map[string]Group{"OPEN": GroupInProgress, "MERGED": GroupDone, "CLOSED": GroupCancelled}[state]
		matching.GroupTiming = &GroupTiming{Group: group, Since: timingChanged}
		g.branches[key] = []PullRequest{foreign, matching}
		catalog, err := app.Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(catalog.PRs) != 1 {
			t.Fatal(catalog)
		}
		p := catalog.PRs[0]
		if p.IsLocal() || p.Key() != matching.Key() || p.Title != "Actual PR title" || !reflect.DeepEqual(p.Links, c.Links) || !reflect.DeepEqual(p.GroupTiming, matching.GroupTiming) {
			t.Fatal(p)
		}
	}
}

func TestCachedLocalBeforeBlockingAuthAndNetwork(t *testing.T) {
	for _, block := range []string{"auth", "network"} {
		t.Run(block, func(t *testing.T) {
			g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
			c := Checkout{Path: "/local", LocalID: "/local", Branch: "topic", LocalBranch: "topic", HeadRepository: "fork/r", Repositories: []string{"up/r"}, Links: []Link{{WorkspaceID: "ws", PaneID: "p"}}}
			a, _ := cacheFixture(t, g, c)
			started, release := make(chan struct{}), make(chan struct{})
			if block == "auth" {
				a.ScopeProvider = func(ctx context.Context, _ GitHub) (string, error) {
					close(started)
					select {
					case <-release:
						return "account", nil
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}
			} else {
				g.branch = func(ctx context.Context, _, _ string) ([]PullRequest, error) {
					close(started)
					select {
					case <-release:
						return nil, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			updates := make(chan CatalogUpdate, 8)
			done := make(chan error, 1)
			go func() { done <- a.Load(ctx, false, func(u CatalogUpdate) { updates <- u }) }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("blocked operation never started")
			}
			select {
			case u := <-updates:
				if u.Cached || len(u.Catalog.PRs) != 1 || !u.Catalog.PRs[0].IsLocal() || u.Catalog.PRs[0].Key() != "local:/local" {
					t.Fatal(u)
				}
			default:
				t.Fatal("auth/network started before local publication")
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestForcedRefreshDoesNotPublishProvisionalLocals(t *testing.T) {
	p := catalogPR("up/r", 1, "fork/r", "topic", "OPEN")
	q := (branchQuery{"up/r", "topic"}).key()
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{mine: []PullRequest{p}, branches: map[string][]PullRequest{q: {p}}}}
	c := Checkout{Path: "/local", LocalID: "/local", Branch: "topic", LocalBranch: "topic", HeadRepository: "fork/r", Repositories: []string{"up/r"}, Links: []Link{{WorkspaceID: "ws", Path: "/local"}}}
	a, now := cacheFixture(t, g, c)
	if err := a.writeCache(context.Background(), "offline-account", cacheRecord{Key: "mine", SavedAt: *now, PRs: []PullRequest{p}}); err != nil {
		t.Fatal(err)
	}
	var updates []CatalogUpdate
	a.ScopeProvider = func(context.Context, GitHub) (string, error) {
		if len(updates) != 0 {
			t.Fatal("refresh replaced current PRs before resolving auth")
		}
		return "offline-account", nil
	}
	if err := a.Load(context.Background(), true, func(u CatalogUpdate) { updates = append(updates, u) }); err != nil {
		t.Fatal(err)
	}
	if len(updates) == 0 {
		t.Fatal("no refresh published")
	}
	for _, u := range updates {
		if len(u.Catalog.PRs) != 1 || u.Catalog.PRs[0].IsLocal() || u.Catalog.PRs[0].Key() != p.Key() {
			t.Fatal("provisional local replaced known PR", u)
		}
	}
	updates = nil
	failure := errors.New("auth unavailable")
	a.ScopeProvider = func(context.Context, GitHub) (string, error) { return "", failure }
	if err := a.Load(context.Background(), true, func(u CatalogUpdate) { updates = append(updates, u) }); err == nil || len(updates) != 0 {
		t.Fatal("forced auth failure flashed provisional locals", err, updates)
	}
}

func TestCachedWorkRowsUseFreshDefaultBranchMetadata(t *testing.T) {
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
	c := Checkout{Path: "/local", LocalID: "/local", Branch: "topic", LocalBranch: "topic", DefaultBranch: "develop", HeadRepository: "o/r", Repositories: []string{"o/r"}, Links: []Link{{PaneID: "p", Path: "/local"}}}
	a, _ := cacheFixture(t, g, c)
	updates := cacheLoad(t, a, false)
	if len(updates[len(updates)-1].Catalog.PRs) != 1 {
		t.Fatal(updates)
	}
	c.DefaultBranch = "topic"
	a.App.Discover = func(context.Context, Snapshot, string, string, string) ([]Checkout, []string, error) {
		return []Checkout{c}, nil, nil
	}
	g.mineHook = func(context.Context) ([]PullRequest, error) { t.Fatal("fresh cache queried Mine"); return nil, nil }
	g.branch = func(context.Context, string, string) ([]PullRequest, error) {
		t.Fatal("fresh cache queried branch")
		return nil, nil
	}
	for _, u := range cacheLoad(t, a, false) {
		if len(u.Catalog.PRs) != 0 {
			t.Fatal("fresh local metadata did not remove default-branch work row", u)
		}
	}
}

func TestCachedLocalRetainedWithoutAuthOrNetwork(t *testing.T) {
	for _, failure := range []string{"auth", "offline"} {
		t.Run(failure, func(t *testing.T) {
			g := &cacheGitHub{catalogGitHub: &catalogGitHub{mineErr: errors.New("offline")}}
			a, now := cacheFixture(t, g, Checkout{Path: "/local", LocalID: "/local", Branch: "feature", LocalBranch: "feature", DefaultBranch: "develop", Title: "Feature task", Links: []Link{{WorkspaceID: "ws"}}})
			if failure == "auth" {
				a.ScopeProvider = func(context.Context, GitHub) (string, error) { return "", errors.New("secret-token") }
			}
			var updates []CatalogUpdate
			err := a.Load(context.Background(), false, func(u CatalogUpdate) { updates = append(updates, u) })
			if err == nil || strings.Contains(err.Error(), "secret-token") || len(updates) < 2 {
				t.Fatal(err, updates)
			}
			for _, u := range updates {
				if len(u.Catalog.PRs) != 1 || !u.Catalog.PRs[0].IsLocal() || len(u.Catalog.PRs[0].Links) != 1 {
					t.Fatal(u)
				}
				if want := (&GroupTiming{Group: GroupInProgress, Since: now.Format(time.RFC3339), Approximate: true}); !reflect.DeepEqual(u.Catalog.PRs[0].GroupTiming, want) {
					t.Fatal("auth/network failure lost local observation clock", u)
				}
				if failure == "auth" && u.Cached {
					t.Fatal("local rows marked stale before account determined", u)
				}
			}
			if calls, _ := g.counts(); failure == "auth" && calls != 0 {
				t.Fatal("Mine called without auth")
			}
		})
	}
}

func TestLocalDetailNeverConstructsGitHubOrUsesCache(t *testing.T) {
	p := localPullRequest(Checkout{Path: "/private-local-path", LocalBranch: "topic", Links: []Link{{WorkspaceID: "ws"}}})
	app := &App{GitHubFactory: func() GitHub { t.Fatal("local detail constructed GitHub"); return nil }}
	d, err := app.Detail(context.Background(), p)
	if err != nil || d.PR.Key() != p.Key() || !strings.Contains(d.Body, p.Local.Path) || !strings.Contains(d.Body, "No linked pull request") || strings.Contains(d.Body, "in progress") {
		t.Fatal(d, err)
	}
	cached := NewCachedApp(app)
	cached.CacheDir = t.TempDir()
	cached.ScopeProvider = func(context.Context, GitHub) (string, error) { t.Fatal("local detail resolved auth"); return "", nil }
	updates := cacheDetail(t, cached, p, true)
	if len(updates) != 1 || updates[0].Cached || updates[0].FromCache || updates[0].Detail.PR.Key() != p.Key() {
		t.Fatal(updates)
	}
	entries, err := os.ReadDir(cached.CacheDir)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	client := NewGitHubClient()
	client.tokenSource = func(context.Context) (string, error) { t.Fatal("local detail resolved GitHub auth"); return "", nil }
	if d, err := client.Detail(context.Background(), p); err != nil || d.PR.Key() != p.Key() {
		t.Fatal(d, err)
	}
}

func TestPersistentCacheFiltersLocalRowsAndRejectsLocalDetail(t *testing.T) {
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
	a, now := cacheFixture(t, g)
	scope, _ := a.scope(context.Background(), g)
	local := localPullRequest(Checkout{Path: "/private-local-path", Links: []Link{{PaneID: "private-pane"}}})
	remote := catalogPR("o/r", 1, "o/r", "topic", "OPEN")
	remote.Links = local.Links
	r := cacheRecord{Key: "mine", SavedAt: *now, PRs: []PullRequest{local, remote}}
	if err := a.writeCache(context.Background(), scope, r); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.cachePath(scope, "mine"))
	if err != nil || strings.Contains(string(data), "private-") || strings.Contains(string(data), `"local"`) {
		t.Fatal("local metadata persisted", string(data), err)
	}
	r.Version = cacheVersion
	data, _ = json.Marshal(r) // Simulate an older or externally poisoned cache.
	if err := os.WriteFile(a.cachePath(scope, "mine"), data, 0600); err != nil {
		t.Fatal(err)
	}
	read, ok := a.readCache(scope, "mine")
	if !ok || len(read.PRs) != 1 || read.PRs[0].IsLocal() || len(read.PRs[0].Links) != 0 {
		t.Fatal(read, ok)
	}
	r.Key, r.PRs, r.Detail = "detail:o/r#1", nil, &Detail{PR: local}
	if err := a.writeCache(context.Background(), scope, r); err == nil {
		t.Fatal("local detail write accepted")
	}
	data, _ = json.Marshal(r)
	if err := os.WriteFile(a.cachePath(scope, r.Key), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.readCache(scope, r.Key); ok {
		t.Fatal("local detail read accepted")
	}
}
