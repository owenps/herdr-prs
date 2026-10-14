package prs

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestCachedCatalogObservationPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		mineAge, branchAge       time.Duration
		cacheMine, cacheBranch   bool
		branchFails, advanceMine bool
		wantMine                 bool
		closedSource             string
		mineCalls, branchCalls   int
	}{
		{name: "fresh mine beats older TTL-valid branch", cacheBranch: true, branchAge: 20 * time.Second, wantMine: true, mineCalls: 1},
		{name: "fresh mine beats retained failed branch", cacheBranch: true, branchAge: 2 * time.Minute, branchFails: true, wantMine: true, mineCalls: 1, branchCalls: 1},
		{name: "live branch beats older cached mine", cacheMine: true, mineAge: 20 * time.Second, branchCalls: 1},
		{name: "later live mine beats live branch", advanceMine: true, wantMine: true, mineCalls: 1, branchCalls: 1},
		{name: "reopen prefers newest cached mine", cacheMine: true, cacheBranch: true, mineAge: 10 * time.Second, branchAge: 20 * time.Second, wantMine: true},
		{name: "reopen prefers newest cached branch", cacheMine: true, cacheBranch: true, mineAge: 20 * time.Second, branchAge: 10 * time.Second},
		{name: "newer closed mine revision beats later open observation", cacheMine: true, cacheBranch: true, mineAge: 20 * time.Second, branchAge: 10 * time.Second, closedSource: "mine", wantMine: true},
		{name: "newer closed branch revision beats later open observation", cacheMine: true, cacheBranch: true, mineAge: 10 * time.Second, branchAge: 20 * time.Second, closedSource: "branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mine := catalogPR("up/r", 1, "fork/r", "topic", "OPEN")
			mine.Title, mine.UpdatedAt, mine.HeadOID = "authored snapshot", "2026-01-01T00:00:00Z", "head"
			mine.FeedbackComplete, mine.ChecksComplete = true, true
			mine.MergeState, mine.Mergeable, mine.ReviewDecision = "CLEAN", "MERGEABLE", "APPROVED"
			mine.Checks = []Check{{Name: "CI", Status: "COMPLETED", Conclusion: "SUCCESS"}}
			branch := mine
			branch.Title = "branch snapshot"
			branch.Checks = []Check{{Name: "CI", Status: "COMPLETED", Conclusion: "FAILURE"}}
			if tc.closedSource != "" {
				mine.UpdatedAt, branch.UpdatedAt = "2025-12-01T00:00:00Z", "2025-12-01T00:00:00Z"
				closed := &branch
				if tc.closedSource == "mine" {
					closed = &mine
				}
				closed.State, closed.UpdatedAt, closed.CompletedAt = "CLOSED", "2025-12-02T00:00:00Z", "2025-12-02T00:00:00Z"
			}
			q := (branchQuery{"up/r", "topic"}).key()
			g := &cacheGitHub{catalogGitHub: &catalogGitHub{mine: []PullRequest{mine}, branches: map[string][]PullRequest{q: {branch}}}}
			failure := errors.New("branch unavailable")
			if tc.branchFails {
				g.branchErrors = map[string]error{q: failure}
			}
			links := []Link{{PaneID: "current", Path: "/current"}}
			a, now := cacheFixture(t, g, Checkout{Branch: "topic", HeadRepository: "fork/r", Repositories: []string{"up/r"}, Links: links})
			scope, err := a.scope(context.Background(), g)
			if err != nil {
				t.Fatal(err)
			}
			seed := func(key string, age time.Duration, p PullRequest) {
				t.Helper()
				// Existing v1 records have only SavedAt, no per-PR observation.
				if err := a.writeCache(context.Background(), scope, cacheRecord{Key: key, SavedAt: now.Add(-age), PRs: []PullRequest{p}}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.cacheMine {
				seed("mine", tc.mineAge, mine)
			}
			if tc.cacheBranch {
				seed("branch:"+q, tc.branchAge, branch)
			}
			if tc.advanceMine {
				g.mineHook = func(context.Context) ([]PullRequest, error) {
					*now = now.Add(time.Second)
					return []PullRequest{mine}, nil
				}
			}
			// Reopening must preserve precedence without any in-memory metadata.
			b := NewCachedApp(a.App)
			b.CacheDir, b.Now, b.ScopeProvider = a.CacheDir, a.Now, a.ScopeProvider
			var updates []CatalogUpdate
			err = b.Load(context.Background(), false, func(u CatalogUpdate) { updates = append(updates, u) })
			if tc.branchFails {
				if !errors.Is(err, failure) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(updates) == 0 {
				t.Fatal("no updates")
			}
			last := updates[len(updates)-1]
			want := branch
			if tc.wantMine {
				want = mine
			}
			want.Links = links
			if !reflect.DeepEqual(last.Catalog.PRs, []PullRequest{want}) || last.Cached != tc.branchFails {
				t.Fatalf("last = %+v; want PR %+v, stale %v", last, want, tc.branchFails)
			}
			if calls, _ := g.counts(); calls != tc.mineCalls || g.calls[q] != tc.branchCalls {
				t.Fatal("unexpected requests", calls, g.calls)
			}
			if !tc.branchFails {
				// Live observation times must survive another popup, too.
				reopened := NewCachedApp(a.App)
				reopened.CacheDir, reopened.Now, reopened.ScopeProvider = a.CacheDir, a.Now, a.ScopeProvider
				again := cacheLoad(t, reopened, false)
				if len(again) < 2 {
					t.Fatal("reopen missing local or authoritative publication", again)
				}
				first, last := again[0], again[len(again)-1]
				if len(first.Catalog.PRs) != 1 || !first.Catalog.PRs[0].IsLocal() || last.Cached || !reflect.DeepEqual(last.Catalog.PRs, []PullRequest{want}) {
					t.Fatal("reopen lost newest snapshot", again)
				}
				if calls, _ := g.counts(); calls != tc.mineCalls || g.calls[q] != tc.branchCalls {
					t.Fatal("reopen fetched fresh sources", calls, g.calls)
				}
			}
		})
	}
}

func TestCachedDetailProvenancePreservesSnapshot(t *testing.T) {
	old := catalogPR("up/r", 1, "fork/r", "topic", "OPEN")
	old.UpdatedAt, old.HeadOID = "updated", "head"
	old.Checks = []Check{{Name: "CI", Conclusion: "FAILURE"}}
	old.ReviewDecision, old.Unresolved = "CHANGES_REQUESTED", 2
	stored := Detail{PR: old, Body: "cached prose", Reviews: []Review{{Body: "review"}}, Threads: []Thread{{Path: "file"}}}
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
	g.detailHook = func(context.Context, PullRequest) (Detail, error) { return stored, nil }
	a, _ := cacheFixture(t, g)
	cacheDetail(t, a, old, false)
	current := old
	current.Links = []Link{{PaneID: "current"}}
	current.Checks, current.ReviewDecision, current.Unresolved = nil, "APPROVED", 0
	updates := cacheDetail(t, a, current, false)
	stored.PR.Links = current.Links
	if len(updates) != 1 || !updates[0].FromCache || updates[0].Cached || !reflect.DeepEqual(updates[0].Detail, stored) {
		t.Fatalf("disk detail/provenance altered: %+v", updates)
	}
	stored.PR = current
	stored.Body = "live prose"
	updates = cacheDetail(t, a, current, true)
	if len(updates) != 2 || !updates[0].FromCache || !updates[0].Cached || updates[1].FromCache || updates[1].Cached || !reflect.DeepEqual(updates[1].Detail, stored) {
		t.Fatalf("live detail/provenance altered: %+v", updates)
	}
}
