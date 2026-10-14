package prs

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestWorkflowGroups(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pr    PullRequest
		group Group
	}{
		{"local", PullRequest{Local: &LocalWork{ID: "work"}}, GroupInProgress},
		{"draft with feedback", PullRequest{State: "OPEN", Draft: true, Unresolved: 2, ReviewDecision: "CHANGES_REQUESTED"}, GroupInProgress},
		{"feedback", PullRequest{State: "OPEN", Unresolved: 1}, GroupInReview},
		{"checks running", PullRequest{State: "OPEN", Checks: []Check{{Status: "IN_PROGRESS"}}}, GroupInReview},
		{"checks failing", PullRequest{State: "OPEN", Checks: []Check{{State: "FAILURE"}}}, GroupInReview},
		{"conflicts", PullRequest{State: "OPEN", Mergeable: "CONFLICTING"}, GroupInReview},
		{"ready", PullRequest{State: "OPEN", ChecksComplete: true, FeedbackComplete: true, MergeState: "CLEAN", Mergeable: "MERGEABLE"}, GroupInReview},
		{"missing lifecycle", PullRequest{}, GroupInReview},
		{"unknown lifecycle", PullRequest{State: "UNKNOWN"}, GroupInReview},
		{"merged overrides draft", PullRequest{State: "MERGED", Draft: true, Unresolved: 1}, GroupDone},
		{"closed overrides draft", PullRequest{State: "CLOSED", Draft: true, Unresolved: 1}, GroupCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.pr.Group(); got != tc.group {
				t.Fatalf("group=%s, want %s", got, tc.group)
			}
			if !strings.Contains(tc.pr.SearchText(), string(tc.group)) {
				t.Fatal("group not searchable", tc.pr.SearchText())
			}
		})
	}
}

func TestCatalogWorkflowOrdering(t *testing.T) {
	groups := []Group{GroupDone, GroupInReview, GroupInProgress, GroupCancelled}
	for order, g := range groups {
		if g.Order() != order {
			t.Fatal(g, g.Order(), order)
		}
	}
	if Group("other").Order() <= GroupCancelled.Order() {
		t.Fatal("unknown group ranked before known groups")
	}
	// Within a group, attention priority precedes identity; equal priority is lexical.
	rows := []PullRequest{
		catalogPR("o/r", 1, "o/r", "draft", "OPEN"),
		catalogPR("o/r", 2, "o/r", "review", "OPEN"),
		catalogPR("o/r", 3, "o/r", "merged", "MERGED"),
		catalogPR("o/r", 4, "o/r", "closed", "CLOSED"),
		catalogPR("o/r", 5, "o/r", "feedback", "OPEN"),
		catalogPR("o/r", 6, "o/r", "feedback", "OPEN"),
	}
	rows[0].Draft = true
	rows[4].Unresolved, rows[5].Unresolved = 1, 1
	var checkouts []Checkout
	g := &catalogGitHub{branches: map[string][]PullRequest{}}
	for _, p := range rows {
		checkouts = append(checkouts, Checkout{HeadRepository: p.HeadRepository, Branch: p.Branch, Repositories: []string{p.Repository}})
		key := (branchQuery{p.Repository, p.Branch}).key()
		g.branches[key] = append(g.branches[key], p)
	}
	catalog, err := catalogApp(g, checkouts...).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range catalog.PRs {
		got = append(got, p.Key())
	}
	if want := []string{"o/r#3", "o/r#5", "o/r#6", "o/r#2", "o/r#1", "o/r#4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog order = %v, want %v", got, want)
	}
}
