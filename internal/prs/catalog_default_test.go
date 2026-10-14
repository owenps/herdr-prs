package prs

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestCatalogDefaultBranchEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, local, mapped, defaultBranch string
		completed                          bool
	}{
		{"main", "main", "main", "main", false},
		{"custom default", "release/stable", "release/stable", "release/stable", false},
		{"actual default mapped to feature", "main", "feature", "main", false},
		{"feature mapped to default", "feature", "main", "main", true},
		{"main is feature", "main", "main", "develop", true},
		{"unknown main", "main", "main", "", true},
		{"unknown master", "master", "master", "", true},
		{"legacy confirmed default", "", "main", "main", false},
		{"legacy feature", "", "feature", "main", true},
		{"legacy unknown default", "", "main", "", true},
	} {
		for _, state := range []string{"OPEN", "MERGED", "CLOSED"} {
			t.Run(tc.name+"/"+state, func(t *testing.T) {
				c := Checkout{Path: "/checkout", LocalBranch: tc.local, Branch: tc.mapped, DefaultBranch: tc.defaultBranch, HeadRepository: "fork/r", Repositories: []string{"up/r"}, Links: []Link{{PaneID: "current"}}}
				p := catalogPR("up/r", 961, "FORK/R", tc.mapped, state)
				p.Draft = true // A closed draft is still completed history.
				p.CreatedAt = timingCreated
				since := timingCreated
				if state != "OPEN" {
					p.CompletedAt, since = timingChanged, timingChanged
				}
				p.GroupTiming = &GroupTiming{Group: p.Group(), Since: since}
				g := &catalogGitHub{mine: []PullRequest{p}, branches: map[string][]PullRequest{(branchQuery{"up/r", tc.mapped}).key(): {p}}}
				catalog, err := catalogApp(g, c).Load(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(g.calls) != 1 {
					t.Fatal("default branch lookup filtered", g.calls)
				}
				if state != "OPEN" && !tc.completed {
					if len(catalog.PRs) != 0 {
						t.Fatal("default history retained or substituted with local row", catalog)
					}
					return
				}
				if len(catalog.PRs) != 1 {
					t.Fatal("eligible PR lost", catalog)
				}
				got := catalog.PRs[0]
				if got.Key() != p.Key() || got.State != state || !got.Draft || got.CompletedAt != p.CompletedAt || !reflect.DeepEqual(got.Links, c.Links) || !reflect.DeepEqual(got.GroupTiming, p.GroupTiming) {
					t.Fatal("eligible PR lifecycle/links/timing changed", got)
				}
			})
		}
	}
}

func TestCatalogIncompleteBranchObservationUsesLocalFallback(t *testing.T) {
	for _, state := range []string{"UNKNOWN", ""} {
		t.Run(state, func(t *testing.T) {
			c := Checkout{Path: "/checkout", LocalBranch: "feature", Branch: "feature", DefaultBranch: "main", HeadRepository: "fork/r", Repositories: []string{"up/r"}, Links: []Link{{PaneID: "current"}}}
			p := catalogPR("up/r", 1, "fork/r", "feature", state)
			g := &catalogGitHub{branches: map[string][]PullRequest{(branchQuery{"up/r", "feature"}).key(): {p}}}
			catalog, err := catalogApp(g, c).Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.PRs) != 1 || !catalog.PRs[0].IsLocal() || catalog.PRs[0].Key() != "local:/checkout" || catalog.PRs[0].Group() != GroupInProgress || !reflect.DeepEqual(catalog.PRs[0].Links, c.Links) {
				t.Fatal("incomplete feature observation lost local fallback", catalog)
			}
		})
	}
}

func TestCatalogSharedDefaultAndFeatureQuery(t *testing.T) {
	for _, state := range []string{"OPEN", "MERGED", "CLOSED"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%v", state, reverse), func(t *testing.T) {
				defaultCheckout := Checkout{Path: "/default", LocalBranch: "main", Branch: "main", DefaultBranch: "main", HeadRepository: "fork/r", Repositories: []string{"up/r"}, Links: []Link{{PaneID: "default"}}}
				feature := defaultCheckout
				feature.Path, feature.LocalBranch, feature.Links = "/feature", "feature", []Link{{PaneID: "feature"}}
				checkouts := []Checkout{defaultCheckout, feature}
				if reverse {
					checkouts[0], checkouts[1] = checkouts[1], checkouts[0]
				}
				p := catalogPR("up/r", 1, "fork/r", "main", state)
				g := &catalogGitHub{branches: map[string][]PullRequest{(branchQuery{"up/r", "main"}).key(): {p}}}
				catalog, err := catalogApp(g, checkouts...).Load(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(g.calls) != 1 {
					t.Fatal("shared query not deduplicated", g.calls)
				}
				if len(catalog.PRs) != 1 || catalog.PRs[0].Key() != p.Key() {
					t.Fatal("shared feature PR lost or foreign head retained", catalog)
				}
				links := catalog.PRs[0].Links
				if state == "OPEN" {
					panes := map[string]bool{}
					for _, link := range links {
						panes[link.PaneID] = true
					}
					if len(links) != 2 || !panes["default"] || !panes["feature"] {
						t.Fatal("genuine default/feature OPEN links lost", links)
					}
				} else if !reflect.DeepEqual(links, feature.Links) {
					t.Fatal("completed PR attached to default", links)
				}
			})
		}
	}
}

func TestCatalogDefaultBranchTerminalSelection(t *testing.T) {
	for _, terminalSource := range []string{"mine", "branch"} {
		for _, newerRevision := range []bool{false, true} {
			for _, withFeature := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/revision=%v/feature=%v", terminalSource, newerRevision, withFeature), func(t *testing.T) {
					now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
					open := catalogPR("up/r", 1, "fork/r", "main", "OPEN")
					open.UpdatedAt = "2025-12-01T00:00:00Z"
					terminal := open
					terminal.State, terminal.CompletedAt = "CLOSED", "2025-12-02T00:00:00Z"
					terminal.Checks = []Check{{Name: "CI", Conclusion: "FAILURE"}}
					terminal.GroupTiming = &GroupTiming{Group: GroupCancelled, Since: terminal.CompletedAt}
					terminalAt, openAt := now, now.Add(-time.Minute)
					if newerRevision {
						terminal.UpdatedAt = terminal.CompletedAt
						terminalAt, openAt = openAt, terminalAt
					}
					mine, branch, mineAt, branchAt := terminal, open, terminalAt, openAt
					if terminalSource == "branch" {
						mine, branch, mineAt, branchAt = open, terminal, openAt, terminalAt
					}
					c := Checkout{LocalBranch: "main", Branch: "main", DefaultBranch: "main", HeadRepository: "fork/r", Repositories: []string{"up/r"}, Links: []Link{{PaneID: "default"}}}
					checkouts := []Checkout{c}
					if withFeature {
						c.LocalBranch, c.Links = "feature", []Link{{PaneID: "feature"}}
						checkouts = append(checkouts, c)
					}
					queries := checkoutQueries(checkouts)
					catalog := assembleCatalog([]PullRequest{mine}, mineAt, checkouts, queries, map[string]branchResult{queries[0].key(): {prs: []PullRequest{branch}, observedAt: branchAt}}, nil)
					if !withFeature {
						if len(catalog.PRs) != 0 {
							t.Fatal("stale authored OPEN resurrected terminal default PR", catalog)
						}
						return
					}
					if len(catalog.PRs) != 1 {
						t.Fatal("feature terminal PR lost", catalog)
					}
					got := catalog.PRs[0]
					if got.Key() != terminal.Key() || got.State != "CLOSED" || got.CompletedAt != terminal.CompletedAt || len(got.Checks) != 1 || got.Checks[0].Conclusion != "FAILURE" || !reflect.DeepEqual(got.Links, c.Links) || !reflect.DeepEqual(got.GroupTiming, terminal.GroupTiming) {
						t.Fatal("feature terminal lifecycle/links/timing altered", got)
					}
				})
			}
		}
	}
}

func TestCachedCatalogFiltersDefaultHistoryPerCheckout(t *testing.T) {
	for _, mode := range []string{"fresh", "stale", "forced"} {
		for _, shared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shared=%v", mode, shared), func(t *testing.T) {
				c := Checkout{Path: "/default", LocalBranch: "main", Branch: "main", DefaultBranch: "main", HeadRepository: "fork/r", Repositories: []string{"up/r"}, Links: []Link{{PaneID: "default"}}}
				feature := c
				feature.Path, feature.LocalBranch, feature.Branch, feature.Links = "/feature", "feature", "feature", []Link{{PaneID: "feature"}}
				if shared {
					feature.Branch = "main"
				}
				legacy := catalogPR("up/r", 961, "fork/r", "main", "CLOSED")
				legacy.Draft, legacy.CompletedAt = true, "2026-03-10T15:12:55Z"
				legacy.GroupTiming = &GroupTiming{Group: GroupCancelled, Since: legacy.CompletedAt}
				retained := legacy
				if !shared {
					retained.Number, retained.Branch, retained.State = 2, "feature", "MERGED"
					retained.GroupTiming = &GroupTiming{Group: GroupDone, Since: retained.CompletedAt}
				}
				authored := catalogPR("elsewhere/r", 3, "elsewhere/r", "main", "OPEN")
				authored.GroupTiming = &GroupTiming{Group: authored.Group()}
				branches := map[string][]PullRequest{(branchQuery{"up/r", "main"}).key(): {legacy}}
				branches[(branchQuery{"up/r", feature.Branch}).key()] = []PullRequest{retained}
				g := &cacheGitHub{catalogGitHub: &catalogGitHub{mine: []PullRequest{legacy, authored}, branches: branches}}
				a, now := cacheFixture(t, g, c, feature)
				scope, err := a.scope(context.Background(), g)
				if err != nil {
					t.Fatal(err)
				}
				savedAt := *now
				if mode == "stale" {
					savedAt = savedAt.Add(-2 * cacheTTL)
				}
				records := []cacheRecord{{Key: "mine", SavedAt: savedAt, PRs: g.mine}}
				for key, prs := range branches {
					records = append(records, cacheRecord{Key: "branch:" + key, SavedAt: savedAt, PRs: prs})
				}
				for _, record := range records {
					if err := a.writeCache(context.Background(), scope, record); err != nil {
						t.Fatal(err)
					}
					if _, ok := a.readCache(scope, record.Key); !ok {
						t.Fatal("seeded record unreadable", record.Key)
					}
				}
				updates := cacheLoad(t, a, mode == "forced")
				for _, u := range updates {
					for _, p := range u.Catalog.PRs {
						for _, link := range p.Links {
							if link.PaneID == "default" {
								t.Fatal("default history/local substitute leaked from cache stage", u)
							}
						}
						if !shared && p.Key() == legacy.Key() {
							t.Fatal("legacy completed default row retained", u)
						}
					}
				}
				last := updates[len(updates)-1].Catalog
				retained.Links = feature.Links
				want := map[string]PullRequest{retained.Key(): retained, authored.Key(): authored}
				if len(last.PRs) != len(want) {
					t.Fatal("feature or authored rows lost", last)
				}
				for _, p := range last.PRs {
					if !reflect.DeepEqual(p, want[p.Key()]) {
						t.Fatal("cached feature snapshot/timing changed", p)
					}
				}
				if mode == "fresh" {
					if calls, _ := g.counts(); calls != 0 || len(g.calls) != 0 {
						t.Fatal("fresh cache queried network", calls, g.calls)
					}
				}
			})
		}
	}
}
