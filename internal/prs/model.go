// Package prs joins read-only GitHub progress to existing Herdr checkouts.
package prs

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type Link struct {
	WorkspaceID string      `json:"workspace_id"`
	TabID       string      `json:"tab_id,omitempty"`
	PaneID      string      `json:"pane_id,omitempty"`
	Agent       bool        `json:"agent"`
	AgentStatus AgentStatus `json:"agent_status,omitempty"`
	Path        string      `json:"path,omitempty"`
	LocalBranch string      `json:"local_branch,omitempty"`
	Label       string      `json:"label,omitempty"`
	Current     bool        `json:"current"`

	// Discovery-proven directory aliases let snapshot refreshes check checkout
	// identity without filesystem or Git reads. Never persisted or rewritten by polls.
	snapshotDirectory     string
	snapshotWorkspacePath string
	snapshotCheckout      string
	snapshotExcludedPane  string
}

type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	State      string `json:"state,omitempty"`
	URL        string `json:"url,omitempty"`
}

// Group is the coarse workflow stage, independent of granular GitHub status.
type Group string

const (
	GroupDone       Group = "Done"
	GroupInReview   Group = "In Review"
	GroupInProgress Group = "In Progress"
	GroupCancelled  Group = "Cancelled"
)

func (g Group) Order() int {
	switch g {
	case GroupDone:
		return 0
	case GroupInReview:
		return 1
	case GroupInProgress:
		return 2
	case GroupCancelled:
		return 3
	default:
		return 4
	}
}

// LocalWork identifies an active feature-branch checkout without a linked PR. Local
// entries never have a GitHub identity and are rebuilt from every snapshot.
type LocalWork struct {
	ID         string `json:"id"`
	Path       string `json:"path,omitempty"`
	Repository string `json:"repository,omitempty"`
}

// GroupTiming records when the current workflow stage began. Since is empty
// when history is unavailable; Approximate marks local first-seen observations.
// The group tag prevents an old timestamp surviving a lifecycle transition.
type GroupTiming struct {
	Group       Group  `json:"group"`
	Since       string `json:"since,omitempty"`
	Approximate bool   `json:"approximate,omitempty"`
}

type PullRequest struct {
	GroupTiming      *GroupTiming `json:"group_timing,omitempty"`
	Local            *LocalWork   `json:"local,omitempty"`
	CreatedAt        string       `json:"created_at,omitempty"`
	Repository       string       `json:"repository"`
	Number           int          `json:"number"`
	Title            string       `json:"title"`
	URL              string       `json:"url"`
	Branch           string       `json:"branch"`
	HeadRepository   string       `json:"head_repository"`
	HeadOID          string       `json:"head_oid"`
	State            string       `json:"state"`
	Draft            bool         `json:"draft"`
	ReviewDecision   string       `json:"review_decision"`
	MergeState       string       `json:"merge_state"`
	Mergeable        string       `json:"mergeable"`
	Checks           []Check      `json:"checks"`
	Unresolved       int          `json:"unresolved"`
	FeedbackComplete bool         `json:"feedback_complete"`
	ChecksComplete   bool         `json:"checks_complete"`
	UpdatedAt        string       `json:"updated_at"`
	CompletedAt      string       `json:"completed_at,omitempty"`
	Links            []Link       `json:"links"`
}

func (p PullRequest) IsLocal() bool { return p.Local != nil }

func (p PullRequest) Key() string {
	if p.IsLocal() {
		return "local:" + p.Local.ID
	}
	return fmt.Sprintf("%s#%d", strings.ToLower(p.Repository), p.Number)
}

// Group is derived, never a second editable/persisted status. Missing GitHub
// lifecycle data remains active with its granular "unknown" status visible;
// it must never imply successful completion or cancellation.
func (p PullRequest) Group() Group {
	if p.IsLocal() {
		return GroupInProgress
	}
	switch p.State {
	case "MERGED":
		return GroupDone
	case "CLOSED":
		return GroupCancelled
	default:
		if p.Draft {
			return GroupInProgress
		}
		return GroupInReview
	}
}

// Status describes GitHub only; local work is represented by Group instead.
func (p PullRequest) Status() string {
	if p.IsLocal() {
		return ""
	}
	if p.State == "MERGED" {
		return "merged"
	}
	if p.State == "CLOSED" {
		return "closed"
	}
	if p.Draft {
		return "draft"
	}
	if p.Unresolved > 0 || p.ReviewDecision == "CHANGES_REQUESTED" {
		return "feedback"
	}
	if p.MergeState == "DIRTY" || p.Mergeable == "CONFLICTING" {
		return "conflicts"
	}
	for _, c := range p.Checks {
		switch c.Conclusion {
		case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE":
			return "checks failing"
		}
		if c.State == "FAILURE" || c.State == "ERROR" {
			return "checks failing"
		}
	}
	for _, c := range p.Checks {
		if c.Status != "" && c.Status != "COMPLETED" || c.State == "PENDING" || c.State == "EXPECTED" {
			return "checks running"
		}
	}
	if p.State != "OPEN" || !p.FeedbackComplete || !p.ChecksComplete {
		return "unknown"
	}
	if p.MergeState == "CLEAN" && p.Mergeable == "MERGEABLE" && p.ReviewDecision != "REVIEW_REQUIRED" {
		return "ready"
	}
	return "in review"
}

func (p PullRequest) Priority() int {
	if p.IsLocal() {
		return 5
	}
	return map[string]int{"draft": 0, "feedback": 1, "checks failing": 2, "conflicts": 3, "ready": 4, "checks running": 5, "in review": 6, "unknown": 7, "merged": 8, "closed": 9}[p.Status()]
}

func (p PullRequest) PreferredLink() (Link, bool) {
	if len(p.Links) == 0 {
		return Link{}, false
	}
	links := append([]Link(nil), p.Links...)
	sort.SliceStable(links, func(i, j int) bool {
		a, b := links[i], links[j]
		if a.Current != b.Current {
			return a.Current
		}
		if a.Agent != b.Agent {
			return a.Agent
		}
		if a.WorkspaceID != b.WorkspaceID {
			return a.WorkspaceID < b.WorkspaceID
		}
		return a.PaneID < b.PaneID
	})
	return links[0], true
}

func (p PullRequest) SearchText() string {
	linkage := "unlinked"
	if len(p.Links) > 0 {
		linkage = "linked"
	}
	parts := []string{p.Repository, p.Title, p.Branch, p.Status(), string(p.Group()), linkage}
	if p.IsLocal() {
		parts = append(parts, p.Local.Path, p.Local.Repository)
	} else {
		parts = append(parts, fmt.Sprintf("#%d", p.Number))
	}
	for _, l := range p.Links {
		parts = append(parts, l.Label, l.Path)
	}
	return strings.Join(parts, " ")
}

type Review struct {
	SubmittedAt string `json:"submitted_at,omitempty"`
	Author      string `json:"author"`
	Body        string `json:"body"`
	State       string `json:"state"`
	URL         string `json:"url"`
}
type Comment struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	URL       string `json:"url"`
	CreatedAt string `json:"created_at"`
}
type Thread struct {
	Path     string    `json:"path"`
	Line     int       `json:"line"`
	Resolved bool      `json:"resolved"`
	Outdated bool      `json:"outdated"`
	Comments []Comment `json:"comments"`
}
type Detail struct {
	PR       PullRequest `json:"pr"`
	Body     string      `json:"body"`
	Reviews  []Review    `json:"reviews"`
	Comments []Comment   `json:"comments"`
	Threads  []Thread    `json:"threads"`
	Warnings []string    `json:"warnings"`
}
type Catalog struct {
	PRs      []PullRequest `json:"prs"`
	Warnings []string      `json:"warnings"`
}

type GitHub interface {
	Mine(context.Context) ([]PullRequest, error)
	ForBranch(context.Context, string, string) ([]PullRequest, error)
	Detail(context.Context, PullRequest) (Detail, error)
	Warnings() []string
}
