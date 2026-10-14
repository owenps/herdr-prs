package prs

import (
	"reflect"
	"strings"
	"testing"
)

func TestPullRequestConservativeStatus(t *testing.T) {
	ready := PullRequest{State: "OPEN", FeedbackComplete: true, ChecksComplete: true, MergeState: "CLEAN", Mergeable: "MERGEABLE", ReviewDecision: "APPROVED"}
	type statusCase struct {
		name   string
		change func(*PullRequest)
		want   string
	}
	tests := []statusCase{
		{"ready", func(*PullRequest) {}, "ready"},
		{"missing feedback", func(p *PullRequest) { p.FeedbackComplete = false }, "unknown"},
		{"missing checks", func(p *PullRequest) { p.ChecksComplete = false }, "unknown"},
		{"missing state", func(p *PullRequest) { p.State = "" }, "unknown"},
		{"unknown lifecycle", func(p *PullRequest) { p.State = "UNKNOWN" }, "unknown"},
		{"unknown mergeability", func(p *PullRequest) { p.Mergeable = "UNKNOWN" }, "in review"},
		{"unknown merge state", func(p *PullRequest) { p.MergeState = "UNKNOWN" }, "in review"},
		{"review required", func(p *PullRequest) { p.ReviewDecision = "REVIEW_REQUIRED" }, "in review"},
		{"draft", func(p *PullRequest) { p.Draft = true }, "draft"},
		{"unresolved", func(p *PullRequest) { p.Unresolved = 1 }, "feedback"},
		{"feedback before conflicts and checks", func(p *PullRequest) {
			p.Unresolved = 1
			p.Mergeable = "CONFLICTING"
			p.Checks = []Check{{State: "FAILURE"}}
		}, "feedback"},
		{"conflicts before checks", func(p *PullRequest) {
			p.Mergeable = "CONFLICTING"
			p.Checks = []Check{{State: "FAILURE"}}
		}, "conflicts"},
		{"failure after running check", func(p *PullRequest) {
			p.Checks = []Check{{Status: "IN_PROGRESS"}, {Conclusion: "FAILURE"}}
		}, "checks failing"},
		{"checks before incomplete metadata", func(p *PullRequest) {
			p.FeedbackComplete, p.ChecksComplete = false, false
			p.Checks = []Check{{Status: "IN_PROGRESS"}}
		}, "checks running"},
		{"draft overrides feedback", func(p *PullRequest) { p.Unresolved = 1; p.Draft = true; p.ReviewDecision = "CHANGES_REQUESTED" }, "draft"},
		{"draft overrides conflicts", func(p *PullRequest) { p.MergeState = "DIRTY"; p.Draft = true }, "draft"},
		{"draft overrides failing checks", func(p *PullRequest) { p.Checks = []Check{{State: "FAILURE"}}; p.Draft = true }, "draft"},
		{"changes requested", func(p *PullRequest) { p.ReviewDecision = "CHANGES_REQUESTED"; p.MergeState = "DIRTY" }, "feedback"},
		{"dirty", func(p *PullRequest) { p.MergeState = "DIRTY" }, "conflicts"},
		{"conflicting", func(p *PullRequest) { p.Mergeable = "CONFLICTING" }, "conflicts"},
		{"merged wins", func(p *PullRequest) { p.State = "MERGED"; p.Unresolved = 1; p.Draft = true }, "merged"},
		{"closed wins", func(p *PullRequest) { p.State = "CLOSED"; p.Unresolved = 1; p.Draft = true }, "closed"},
	}
	for _, conclusion := range []string{"FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE"} {
		conclusion := conclusion
		tests = append(tests, statusCase{conclusion, func(p *PullRequest) {
			p.Checks = []Check{{Status: "COMPLETED", Conclusion: conclusion}}
		}, "checks failing"})
	}
	for _, state := range []string{"FAILURE", "ERROR", "PENDING", "EXPECTED"} {
		state := state
		want := "checks running"
		if state == "FAILURE" || state == "ERROR" {
			want = "checks failing"
		}
		tests = append(tests, statusCase{state, func(p *PullRequest) { p.Checks = []Check{{State: state}} }, want})
	}
	for _, status := range []string{"QUEUED", "IN_PROGRESS", "WAITING", "UNKNOWN"} {
		status := status
		tests = append(tests, statusCase{status, func(p *PullRequest) { p.Checks = []Check{{Status: status}} }, "checks running"})
	}
	for _, conclusion := range []string{"SUCCESS", "NEUTRAL", "SKIPPED"} {
		conclusion := conclusion
		tests = append(tests, statusCase{conclusion, func(p *PullRequest) { p.Checks = []Check{{Status: "COMPLETED", Conclusion: conclusion}} }, "ready"})
	}
	priorities := map[string]int{"draft": 0, "feedback": 1, "checks failing": 2, "conflicts": 3, "ready": 4, "checks running": 5, "in review": 6, "unknown": 7, "merged": 8, "closed": 9}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ready
			tt.change(&p)
			if got := p.Status(); got != tt.want {
				t.Fatalf("status = %q, want %q", got, tt.want)
			}
			if got := p.Priority(); got != priorities[tt.want] {
				t.Fatalf("priority = %d, want %d", got, priorities[tt.want])
			}
		})
	}
}

func TestPullRequestPreferredLink(t *testing.T) {
	current := Link{WorkspaceID: "z", PaneID: "z", Current: true}
	agent := Link{WorkspaceID: "z", PaneID: "z", Agent: true}
	first := Link{WorkspaceID: "a", PaneID: "a"}
	for _, tt := range []struct {
		name  string
		links []Link
		want  Link
		ok    bool
	}{
		{"none", nil, Link{}, false},
		{"current before agent", []Link{agent, first, current}, current, true},
		{"agent before workspace", []Link{first, agent}, agent, true},
		{"workspace and pane tie break", []Link{{WorkspaceID: "b"}, {WorkspaceID: "a", PaneID: "b"}, first}, first, true},
		{"current agent", []Link{current, {WorkspaceID: "zz", Current: true, Agent: true}}, Link{WorkspaceID: "zz", Current: true, Agent: true}, true},
		{"stable equal rank", []Link{{WorkspaceID: "a", PaneID: "a", Label: "first"}, {WorkspaceID: "a", PaneID: "a", Label: "second"}}, Link{WorkspaceID: "a", PaneID: "a", Label: "first"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := append([]Link(nil), tt.links...)
			p := PullRequest{Links: tt.links}
			got, ok := p.PreferredLink()
			if got != tt.want || ok != tt.ok {
				t.Fatalf("preferred = %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
			if !reflect.DeepEqual(before, p.Links) {
				t.Fatal("PreferredLink mutated links")
			}
		})
	}
}

func TestPullRequestIdentityAndSearchText(t *testing.T) {
	p := PullRequest{Repository: "Owner/Repo", Number: 42, Title: "Fix thing", Branch: "Topic", State: "CLOSED"}
	if p.Key() != "owner/repo#42" {
		t.Fatal(p.Key())
	}
	for _, text := range []string{"Owner/Repo", "#42", "Fix thing", "Topic", "closed", "unlinked"} {
		if !strings.Contains(p.SearchText(), text) {
			t.Errorf("search missing %q", text)
		}
	}
	p.Links = []Link{{Label: "my workspace", Path: "/offline/repo"}}
	if text := p.SearchText(); strings.Contains(text, "unlinked") || !strings.Contains(text, "linked") || !strings.Contains(text, "my workspace") || !strings.Contains(text, "/offline/repo") {
		t.Fatal(text)
	}
}
