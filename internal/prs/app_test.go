package prs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// All access during concurrent branch lookups is protected. No CLI or API calls.
type catalogGitHub struct {
	mu           sync.Mutex
	mine         []PullRequest
	mineErr      error
	branches     map[string][]PullRequest
	branchErrors map[string]error
	warnings     []string
	calls        map[string]int
	branch       func(context.Context, string, string) ([]PullRequest, error)
}

func (g *catalogGitHub) Mine(ctx context.Context) ([]PullRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return append([]PullRequest(nil), g.mine...), g.mineErr
}
func (g *catalogGitHub) ForBranch(ctx context.Context, repo, branch string) ([]PullRequest, error) {
	g.mu.Lock()
	key := (branchQuery{repo, branch}).key()
	if g.calls == nil {
		g.calls = map[string]int{}
	}
	g.calls[key]++
	prs, err, hook := append([]PullRequest(nil), g.branches[key]...), g.branchErrors[key], g.branch
	g.mu.Unlock()
	if hook != nil {
		return hook(ctx, repo, branch)
	}
	return prs, err
}
func (g *catalogGitHub) Detail(_ context.Context, p PullRequest) (Detail, error) {
	return Detail{PR: p, Body: "offline detail"}, nil
}
func (g *catalogGitHub) Warnings() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.warnings...)
}

type catalogSession struct {
	snapshot Snapshot
	err      error
}

func (s catalogSession) Snapshot(context.Context) (Snapshot, error) { return s.snapshot, s.err }
func catalogApp(g *catalogGitHub, checkouts ...Checkout) *App {
	return &App{Herdr: catalogSession{}, GitHubFactory: func() GitHub { return g }, Discover: func(context.Context, Snapshot, string, string, string) ([]Checkout, []string, error) {
		return checkouts, nil, nil
	}}
}
func catalogPR(repo string, number int, head, branch, state string) PullRequest {
	return PullRequest{Repository: repo, Number: number, HeadRepository: head, Branch: branch, State: state}
}
func catalogKeys(c Catalog) []string {
	keys := make([]string, 0, len(c.PRs))
	for _, p := range c.PRs {
		keys = append(keys, p.Key())
	}
	sort.Strings(keys)
	return keys
}
func catalogWarning(warnings []string, text string) bool {
	for _, w := range warnings {
		if strings.Contains(w, text) {
			return true
		}
	}
	return false
}

func TestAppCatalogDeduplicatesIdentityAndRebuildsLinks(t *testing.T) {
	link := Link{WorkspaceID: "ws", PaneID: "pane", Path: "/offline", LocalBranch: "local"}
	matching := catalogPR("upstream/repo", 7, "FORK/Repo", "Topic", "OPEN")
	duplicate := matching
	duplicate.Repository = "UPSTREAM/REPO"
	otherRepo := matching
	otherRepo.Repository = "other/repo" // Same number in another base repository is distinct.
	unlinked := catalogPR("mine/repo", 1, "mine/repo", "elsewhere", "UNKNOWN")
	unlinked.Links = []Link{{PaneID: "stale"}}
	g := &catalogGitHub{mine: []PullRequest{unlinked, matching, duplicate, otherRepo}, branches: map[string][]PullRequest{
		(branchQuery{"upstream/repo", "Topic"}).key(): {matching, duplicate},
	}}
	c, err := catalogApp(g, Checkout{Branch: "Topic", HeadRepository: "fork/repo", Repositories: []string{"upstream/repo", "UPSTREAM/REPO"}, Links: []Link{link, link}}).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"mine/repo#1", "other/repo#7", "upstream/repo#7"}; !reflect.DeepEqual(catalogKeys(c), want) {
		t.Fatalf("keys = %v, want %v", catalogKeys(c), want)
	}
	for _, p := range c.PRs {
		if p.Key() == unlinked.Key() {
			if len(p.Links) != 0 {
				t.Errorf("stale authored links retained: %+v", p.Links)
			}
		} else if !reflect.DeepEqual(p.Links, []Link{link}) {
			t.Errorf("%s links = %+v", p.Key(), p.Links)
		}
	}
	if len(g.calls) != 1 || g.calls[(branchQuery{"upstream/repo", "Topic"}).key()] != 1 {
		t.Fatal("branch queries not deduplicated", g.calls)
	}
	if unlinked.Links[0].PaneID != "stale" {
		t.Fatal("Mine input mutated")
	}
}

func TestAppCatalogHeadIdentityAndAuthoredRetention(t *testing.T) {
	for _, state := range []string{"OPEN", "UNKNOWN", ""} {
		for _, identity := range []struct {
			name, head, branch string
			matches            bool
		}{
			{"matching fork", "FORK/R", "main", true},
			{"foreign fork", "other/r", "main", false},
			{"branch case mismatch", "fork/r", "Main", false},
			{"missing head", "", "main", false},
		} {
			for _, authored := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/authored=%v", state, identity.name, authored), func(t *testing.T) {
					checkout := Checkout{Path: "/checkout", LocalBranch: "main", Branch: "main", DefaultBranch: "main", HeadRepository: "fork/r", Repositories: []string{"up/r"}, Links: []Link{{PaneID: "default"}}}
					p := catalogPR("up/r", 1, identity.head, identity.branch, state)
					p.Title, p.URL = "PR title", "https://github.com/up/r/pull/1"
					p.Links = []Link{{PaneID: "stale"}}
					g := &catalogGitHub{branches: map[string][]PullRequest{(branchQuery{"up/r", "main"}).key(): {p}}}
					if authored {
						g.mine = []PullRequest{p}
					}
					catalog, err := catalogApp(g, checkout).Load(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					// Incomplete branch-only observations are not active PR evidence.
					retained := authored || state == "OPEN" && identity.matches
					if !retained {
						if len(catalog.PRs) != 0 {
							t.Fatal("unproven branch-only row retained", catalog)
						}
						return
					}
					if len(catalog.PRs) != 1 {
						t.Fatal("authored/eligible PR lost", catalog)
					}
					got := catalog.PRs[0]
					if got.Key() != p.Key() || got.Title != p.Title || got.URL != p.URL || got.State != state || got.CompletedAt != "" || got.Group() != GroupInReview {
						t.Fatal("PR identity/lifecycle changed", got)
					}
					var wantLinks []Link
					if identity.matches {
						wantLinks = checkout.Links
					}
					if !reflect.DeepEqual(got.Links, wantLinks) {
						t.Fatal("head identity links", got.Links, wantLinks)
					}
					if p.Links[0].PaneID != "stale" {
						t.Fatal("source mutated")
					}
				})
			}
		}
	}
}

func TestAppCompletionSelectionGlobalAcrossQueriesAndCheckouts(t *testing.T) {
	for _, separate := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("separate=%v/reverse=%v", separate, reverse), func(t *testing.T) {
				old := catalogPR("a/repo", 1, "fork/repo", "topic", "MERGED")
				old.CompletedAt = "2025-01-01T00:00:00Z"
				old.UpdatedAt = "2026-01-01T00:00:00Z"
				latest := catalogPR("b/repo", 2, "fork/repo", "topic", "CLOSED")
				latest.CompletedAt = "2025-03-01T00:00:00Z"
				fallback := catalogPR("b/repo", 3, "fork/repo", "topic", "MERGED")
				fallback.UpdatedAt = "2025-02-01T00:00:00Z"
				g := &catalogGitHub{branches: map[string][]PullRequest{(branchQuery{"a/repo", "topic"}).key(): {old}, (branchQuery{"b/repo", "topic"}).key(): {latest, fallback}}}
				first := Checkout{Path: "/first", Branch: "topic", HeadRepository: "fork/repo", Repositories: []string{"a/repo", "b/repo"}, Links: []Link{{PaneID: "first"}}}
				checkouts := []Checkout{first}
				if separate {
					checkouts[0].Repositories = []string{"a/repo"}
					second := first
					second.Path = "/second"
					second.Repositories = []string{"b/repo"}
					second.Links = []Link{{PaneID: "second"}}
					checkouts = append(checkouts, second)
				}
				if reverse {
					if separate {
						checkouts[0], checkouts[1] = checkouts[1], checkouts[0]
					} else {
						checkouts[0].Repositories = []string{"b/repo", "a/repo"}
					}
				}
				c, err := catalogApp(g, checkouts...).Load(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if got := catalogKeys(c); !reflect.DeepEqual(got, []string{"b/repo#2"}) {
					t.Fatalf("globally latest completed only: got %v", got)
				}
				wantLinks := 1
				if separate {
					wantLinks = 2
				}
				if len(c.PRs[0].Links) != wantLinks {
					t.Fatalf("latest links = %+v", c.PRs[0].Links)
				}
			})
		}
	}
}

func TestAppAllOpenPreferredToCompletedAndCompletionTie(t *testing.T) {
	for _, open := range []bool{false, true} {
		t.Run(fmt.Sprint(open), func(t *testing.T) {
			first := catalogPR("a/repo", 1, "fork/repo", "topic", "MERGED")
			first.CompletedAt = "2025-01-01T00:00:00Z"
			second := catalogPR("b/repo", 2, "fork/repo", "topic", "CLOSED")
			second.CompletedAt = first.CompletedAt
			prs := []PullRequest{second, first}
			want := []string{"a/repo#1"}
			if open {
				prs = append(prs, catalogPR("a/repo", 3, "fork/repo", "topic", "OPEN"), catalogPR("b/repo", 4, "fork/repo", "topic", "OPEN"))
				want = []string{"a/repo#3", "b/repo#4"}
			}
			g := &catalogGitHub{branches: map[string][]PullRequest{}}
			for _, p := range prs {
				key := (branchQuery{p.Repository, "topic"}).key()
				g.branches[key] = append(g.branches[key], p)
			}
			c, err := catalogApp(g, Checkout{Branch: "topic", HeadRepository: "fork/repo", Repositories: []string{"b/repo", "a/repo"}, Links: []Link{{PaneID: "target"}}}).Load(context.Background())
			if err != nil || !reflect.DeepEqual(catalogKeys(c), want) {
				t.Fatalf("got %v, %v; want %v", catalogKeys(c), err, want)
			}
		})
	}
}

func TestAppDeduplicatedQueriesUseFourConcurrentWorkers(t *testing.T) {
	started := make(chan string, 8)
	release := make(chan struct{})
	g := &catalogGitHub{branch: func(ctx context.Context, repo, branch string) ([]PullRequest, error) {
		started <- repo + "/" + branch
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	checkouts := []Checkout{{Path: "/first", Branch: "topic", HeadRepository: "fork/r", Repositories: []string{"a/r", "b/r", "c/r", "d/r", "e/r", "A/R"}}, {Path: "/second", Branch: "topic", HeadRepository: "fork/r", Repositories: []string{"a/r"}}, {Path: "/third", Branch: "Topic", HeadRepository: "fork/r", Repositories: []string{"a/r"}}}
	done := make(chan struct{})
	var loadErr error
	go func() { _, loadErr = catalogApp(g, checkouts...).Load(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("Load did not stop")
		}
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("four branch workers did not start")
		}
	}
	select {
	case extra := <-started:
		t.Fatalf("more than four concurrent lookups: %s", extra)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
		if loadErr != nil {
			t.Fatal(loadErr)
		}
	case <-ctx.Done():
		t.Fatal("Load blocked")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.calls) != 6 {
		t.Fatalf("queries = %v; want six distinct repo/branch identities", g.calls)
	}
	for q, count := range g.calls {
		if count != 1 {
			t.Errorf("%q called %d times", q, count)
		}
	}
}

func TestAppWarningsAndErrors(t *testing.T) {
	mine := catalogPR("mine/repo", 1, "", "topic", "OPEN")
	g := &catalogGitHub{mine: []PullRequest{mine}, warnings: []string{"partial checks", "partial checks"}}
	a := catalogApp(g)
	a.Herdr = catalogSession{err: errors.New("offline socket")}
	a.Discover = func(context.Context, Snapshot, string, string, string) ([]Checkout, []string, error) {
		t.Error("discovery after snapshot failure")
		return nil, nil, nil
	}
	c, err := a.Load(context.Background())
	if err != nil || len(c.PRs) != 1 || len(c.Warnings) != 2 || !catalogWarning(c.Warnings, "Herdr linkage unavailable: offline socket") || !catalogWarning(c.Warnings, "partial checks") {
		t.Fatalf("snapshot warning: %+v, %v", c, err)
	}
	g.branchErrors = map[string]error{(branchQuery{"a/repo", "topic"}).key(): errors.New("lookup failed")}
	a = catalogApp(g)
	a.Discover = func(context.Context, Snapshot, string, string, string) ([]Checkout, []string, error) {
		return []Checkout{{Path: "/local", Branch: "topic", HeadRepository: "fork/repo", Repositories: []string{"a/repo"}}}, []string{"ambiguous checkout", "ambiguous checkout"}, nil
	}
	c, err = a.Load(context.Background())
	if err != nil || !reflect.DeepEqual(catalogKeys(c), []string{"local:/local", "mine/repo#1"}) || len(c.Warnings) != 3 || !catalogWarning(c.Warnings, "a/repo (topic): lookup failed") || !catalogWarning(c.Warnings, "ambiguous checkout") {
		t.Fatalf("branch/discovery warnings: %+v, %v", c, err)
	}
	failure := errors.New("mine unavailable")
	g.mineErr = failure
	if _, err := a.Load(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("Mine error = %v", err)
	}
	g.mineErr = nil
	a.Discover = func(context.Context, Snapshot, string, string, string) ([]Checkout, []string, error) {
		return nil, nil, failure
	}
	if _, err := a.Load(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("discovery error = %v", err)
	}
	if d, err := a.Detail(context.Background(), mine); err != nil || d.PR.Key() != mine.Key() || d.Body != "offline detail" {
		t.Fatalf("detail delegation = %+v, %v", d, err)
	}
}

func TestAppCancellation(t *testing.T) {
	for _, where := range []string{"before discovery", "snapshot", "discovery", "branch"} {
		t.Run(where, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			g := &catalogGitHub{}
			a := catalogApp(g, Checkout{Path: "/local", Branch: "topic", HeadRepository: "fork/repo", Repositories: []string{"a/repo", "b/repo", "c/repo", "d/repo", "e/repo"}})
			switch where {
			case "before discovery":
				cancel()
			case "snapshot":
				a.Herdr = catalogCancelSession{cancel: cancel}
			case "discovery":
				a.Discover = func(context.Context, Snapshot, string, string, string) ([]Checkout, []string, error) {
					cancel()
					return nil, nil, context.Canceled
				}
			case "branch":
				g.branch = func(ctx context.Context, _, _ string) ([]PullRequest, error) {
					cancel()
					<-ctx.Done()
					return nil, ctx.Err()
				}
			}
			if _, err := a.Load(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation = %v", err)
			}
		})
	}
}

type catalogCancelSession struct{ cancel context.CancelFunc }

func (s catalogCancelSession) Snapshot(context.Context) (Snapshot, error) {
	s.cancel()
	return Snapshot{}, context.Canceled
}

func catalogUnsetEnv(t *testing.T, key string) {
	t.Helper()
	value, existed := os.LookupEnv(key)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, value)
		} else {
			_ = os.Unsetenv(key)
		}
	})
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}
func TestNewAppOriginAndPopupEnvironment(t *testing.T) {
	for _, opened := range []bool{false, true} {
		t.Run(fmt.Sprint(opened), func(t *testing.T) {
			t.Setenv("HERDR_PANE_ID", "popup")
			t.Setenv("HERDR_WORKSPACE_ID", "popup-workspace")
			t.Setenv("HERDR_PRS_ORIGIN_WORKSPACE", "origin-workspace")
			if opened {
				t.Setenv("HERDR_PRS_ORIGIN_PANE", "origin")
			} else {
				catalogUnsetEnv(t, "HERDR_PRS_ORIGIN_PANE")
			}
			a := NewApp(&Herdr{})
			wantPane, wantWorkspace, wantPopup := "popup", "popup-workspace", ""
			if opened {
				wantPane, wantWorkspace, wantPopup = "origin", "origin-workspace", "popup"
			}
			if a.OriginPane != wantPane || a.OriginWorkspace != wantWorkspace || a.PopupPane != wantPopup {
				t.Fatalf("origins = %q %q %q", a.OriginPane, a.OriginWorkspace, a.PopupPane)
			}
		})
	}
	t.Run("empty captured pane remains intentional", func(t *testing.T) {
		t.Setenv("HERDR_PANE_ID", "popup")
		t.Setenv("HERDR_PRS_ORIGIN_PANE", "")
		t.Setenv("HERDR_PRS_ORIGIN_WORKSPACE", "origin-workspace")
		a := NewApp(&Herdr{})
		if a.OriginPane != "" || a.OriginWorkspace != "origin-workspace" || a.PopupPane != "popup" {
			t.Fatalf("empty captured origin ignored: %+v", a)
		}
	})
	t.Run("actual discovery excludes popup", func(t *testing.T) {
		root := herdrGitRepo(t)
		herdrGit(t, root, "remote", "add", "origin", "https://github.com/fork/repo.git")
		t.Setenv("HERDR_PANE_ID", "popup")
		t.Setenv("HERDR_PRS_ORIGIN_PANE", "origin")
		t.Setenv("HERDR_PRS_ORIGIN_WORKSPACE", "ws")
		a := NewApp(&Herdr{})
		a.Herdr = catalogSession{snapshot: Snapshot{Panes: []Pane{{ID: "popup", WorkspaceID: "ws", CWD: root}, {ID: "origin", WorkspaceID: "ws", CWD: root}}}}
		g := &catalogGitHub{branches: map[string][]PullRequest{(branchQuery{"fork/repo", "local"}).key(): {catalogPR("fork/repo", 1, "fork/repo", "local", "OPEN")}}}
		a.GitHubFactory = func() GitHub { return g }
		c, err := a.Load(context.Background())
		if err != nil || len(c.PRs) != 1 || len(c.PRs[0].Links) != 1 || c.PRs[0].Links[0].PaneID != "origin" || !c.PRs[0].Links[0].Current {
			t.Fatalf("popup/origin linkage = %+v, %v", c, err)
		}
	})
}
