package prs

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestCatalogDefaultBranchReopenedPR(t *testing.T) {
	// Eligibility follows the resolved lifecycle, not a permanent terminal-state bias.
	// Controlled source clocks belong at the aggregation seam, not wall-clock Load.
	for _, source := range []string{"mine", "branch"} {
		for _, revision := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/newerRevision=%v", source, revision), func(t *testing.T) {
				now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				closed := catalogPR("up/r", 1, "fork/r", "main", "CLOSED")
				closed.CreatedAt, closed.UpdatedAt, closed.CompletedAt = timingCreated, timingChanged, timingChanged
				closed.GroupTiming = &GroupTiming{Group: GroupCancelled, Since: timingChanged}
				open := closed
				open.Title, open.State, open.CompletedAt = "Reopened PR", "OPEN", ""
				open.ChecksComplete, open.FeedbackComplete = true, true
				open.GroupTiming = &GroupTiming{Group: GroupInReview, Since: "2025-03-01T00:00:00Z"}
				openAt, closedAt := now, now.Add(-time.Minute)
				if revision {
					open.UpdatedAt = "2025-03-01T00:00:00Z"
					openAt, closedAt = closedAt, openAt
				}
				mine, branch, mineAt, branchAt := open, closed, openAt, closedAt
				if source == "branch" {
					mine, branch, mineAt, branchAt = closed, open, closedAt, openAt
				}
				checkout := Checkout{LocalBranch: "main", Branch: "main", DefaultBranch: "main", HeadRepository: "fork/r", Repositories: []string{"up/r"}, Links: []Link{{PaneID: "default"}}}
				queries := checkoutQueries([]Checkout{checkout})
				catalog := assembleCatalog([]PullRequest{mine}, mineAt, []Checkout{checkout}, queries, map[string]branchResult{queries[0].key(): {prs: []PullRequest{branch}, observedAt: branchAt}}, nil)
				if len(catalog.PRs) != 1 {
					t.Fatal("reopened default PR lost", catalog)
				}
				got := catalog.PRs[0]
				if got.Key() != open.Key() || got.State != "OPEN" || got.CompletedAt != "" || got.Title != open.Title || !got.ChecksComplete || !got.FeedbackComplete || !reflect.DeepEqual(got.Links, checkout.Links) || !reflect.DeepEqual(got.GroupTiming, open.GroupTiming) {
					t.Fatal("stale terminal snapshot won", got)
				}
			})
		}
	}
}
