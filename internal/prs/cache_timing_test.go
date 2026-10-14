package prs

import (
	"context"
	"testing"
)

func TestCachedTimingLegacyEnrichedOnceAndUnknownFresh(t *testing.T) {
	for _, legacy := range []string{"missing", "mismatched"} {
		t.Run(legacy, func(t *testing.T) {
			p := catalogPR("o/r", 1, "o/r", "topic", "OPEN")
			p.CreatedAt = timingCreated
			if legacy == "mismatched" {
				p.GroupTiming = &GroupTiming{Group: GroupDone, Since: timingChanged}
			}
			q := (branchQuery{"o/r", "topic"}).key()
			g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
			a, now := cacheFixture(t, g, Checkout{Branch: "topic", HeadRepository: "o/r", Repositories: []string{"o/r"}})
			scope, _ := a.scope(context.Background(), g)
			for _, key := range []string{"mine", "branch:" + q} {
				if err := a.writeCache(context.Background(), scope, cacheRecord{Key: key, SavedAt: *now, PRs: []PullRequest{p}}); err != nil {
					t.Fatal(err)
				}
			}
			key := detailCacheKey(p)
			if err := a.writeCache(context.Background(), scope, cacheRecord{Key: key, SavedAt: *now, Detail: &Detail{PR: p, Body: "cached prose"}}); err != nil {
				t.Fatal(err)
			}
			fresh := p
			fresh.GroupTiming = &GroupTiming{Group: p.Group()} // Attempted but unknown is complete enrichment.
			g.mine, g.branches = []PullRequest{fresh}, map[string][]PullRequest{q: {fresh}}
			g.detailHook = func(context.Context, PullRequest) (Detail, error) { return Detail{PR: fresh, Body: "fresh prose"}, nil }
			updates := cacheLoad(t, a, false)
			if len(updates) < 3 || !updates[1].Cached || updates[len(updates)-1].Cached {
				t.Fatal(updates)
			}
			details := cacheDetail(t, a, fresh, false)
			if len(details) != 2 || details[0].Detail.Body != "cached prose" || !details[0].FromCache || !details[0].Cached || details[1].FromCache {
				t.Fatal(details)
			}
			b := NewCachedApp(a.App)
			b.CacheDir, b.Now, b.ScopeProvider = a.CacheDir, a.Now, a.ScopeProvider
			cacheLoad(t, b, false)
			details = cacheDetail(t, b, fresh, false)
			if len(details) != 1 || details[0].Cached || !details[0].FromCache {
				t.Fatal(details)
			}
			if mine, detail := g.counts(); mine != 1 || detail != 1 || g.calls[q] != 1 {
				t.Fatal("repeat enrichment", mine, detail, g.calls)
			}
		})
	}
}
