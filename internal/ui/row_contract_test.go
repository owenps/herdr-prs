package ui

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/cellbuf"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

func TestReviewStatusHiddenOnlyInListAndBlankStatusCellAligned(t *testing.T) {
	waiting := fixture(1)
	waiting.Title = "Pending review"
	otherWaiting := fixture(2)
	otherWaiting.Title = "Another pending review"
	now := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
	if waiting.Status() != "in review" || rowStatus(waiting) != "" {
		t.Fatal("in review must remain domain status, with an empty list status")
	}
	allWaiting := listRowLayout([]prs.PullRequest{waiting, otherWaiting}, 90, now)
	if allWaiting.status != 0 {
		t.Fatal("review-only list reserved a redundant badge column")
	}
	running, unknown := fixture(3), fixture(4)
	running.Checks = []prs.Check{{Status: "IN_PROGRESS"}}
	unknown.State = ""
	rows := []prs.PullRequest{waiting, running, unknown}
	layout := listRowLayout(rows, 90, now)
	var repoColumn int
	for i, p := range rows {
		out := ansi.Strip(layout.render(p, now, false))
		ref := strings.Index(out, "repo #")
		if ref < 0 || strings.Contains(out, "in review") {
			t.Fatal("review badge suppression lost identity or invented substatus: " + out)
		}
		column := ansi.StringWidth(out[:ref])
		if repoColumn != 0 && column != repoColumn {
			t.Fatal("empty review badge shifted the repository column: " + out)
		}
		repoColumn = column
		badge := []string{"", "checks running", "unknown"}[i]
		if badge != "" && !strings.Contains(out, badge) {
			t.Fatalf("lost granular badge %q: %q", badge, out)
		}
	}

	m := wideModel(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		return prs.Detail{PR: p, Body: "Preview body"}, nil
	}}, waiting, localFixture("local", "/work/local"))
	typeQuery(m, "in review")
	if len(m.rows) != 1 || chosen(m) != waiting.Number || m.rows[0].Status() != "in review" {
		t.Fatal("hiding list badge changed review search/domain status")
	}
	if strings.Contains(ansi.Strip(strings.Join(m.paneBody(90, false), "\n")), "in review") {
		t.Fatal("review badge still shown in filtered list")
	}
	_, render := m.Update(previewRequest(t, m)())
	finishRender(t, m, render)
	preview := ansi.Strip(strings.Join(m.detailLines(), "\n"))
	if !strings.Contains(preview, "in review · owner/repo #1") {
		t.Fatal("list suppression leaked into preview: " + preview)
	}
	finishRender(t, m, press(m, tea.KeyCtrlD))
	if !strings.Contains(plain(m), "in review · owner/repo #1") {
		t.Fatal("review status lost from full detail view: " + plain(m))
	}
}

func TestRowBadgesColorOnlyStatusAndKeepSelectionOnNeutralMetadata(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	for _, tc := range []struct {
		status, color string
		mutate        func(*prs.PullRequest)
	}{
		{"feedback", "#d08770", func(p *prs.PullRequest) { p.Unresolved = 1 }},
		{"conflicts", "#bf616a", func(p *prs.PullRequest) { p.MergeState = "DIRTY" }},
		{"checks failing", "#bf616a", func(p *prs.PullRequest) { p.Checks = []prs.Check{{Conclusion: "FAILURE"}} }},
		{"checks running", "#81a1c1", func(p *prs.PullRequest) { p.Checks = []prs.Check{{Status: "IN_PROGRESS"}} }},
		{"draft", "#9099aa", func(p *prs.PullRequest) { p.Draft = true }},
		{"ready", "#a3be8c", func(p *prs.PullRequest) { p.MergeState = "CLEAN" }},
		{"unknown", "#9099aa", func(p *prs.PullRequest) { p.State = "" }},
		{"merged", "#b48ead", func(p *prs.PullRequest) { p.State = "MERGED" }},
		{"closed", "#9099aa", func(p *prs.PullRequest) { p.State = "CLOSED" }},
	} {
		t.Run(tc.status, func(t *testing.T) {
			p := fixture(123)
			p.Title = "**Literal neutral title**"
			p.GroupTiming = &prs.GroupTiming{Group: p.Group(), Since: "2026-01-01T00:00:00Z"}
			p.Links = []prs.Link{{WorkspaceID: "workspace", Path: "/work/repo", Current: true}}
			tc.mutate(&p)
			if p.Status() != tc.status || rowStatus(p) != tc.status {
				t.Fatalf("granular status changed: domain=%q list=%q", p.Status(), rowStatus(p))
			}
			p.GroupTiming.Group = p.Group()
			now := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
			layout := listRowLayout([]prs.PullRequest{p}, 100, now)
			for _, selected := range []bool{false, true} {
				out := layout.render(p, now, selected)
				badge := assertRenderedForeground(t, out, tc.status, tc.color)
				if badge.Attrs&cellbuf.BoldAttr == 0 {
					t.Fatal("granular badge lost emphasis")
				}
				if !reflect.DeepEqual(badge.Bg, styleAt(t, out, p.Title).Bg) {
					t.Fatal("status badge reset the row's selection background")
				}
				for _, token := range []string{p.Title, "3d", "repo #123↗*", p.Branch} {
					pen := styleAt(t, out, token)
					if pen.Fg != nil || (pen.Bg != nil) != selected {
						t.Fatalf("badge foreground/selection leaked into %q: %+v", token, pen)
					}
					if token == p.Title && (pen.Attrs&cellbuf.BoldAttr != 0) != selected {
						t.Fatal("selected title emphasis missing or leaked")
					}
				}
			}
		})
	}
	local := localFixture("local", "/work/checkout")
	local.GroupTiming = &prs.GroupTiming{Group: local.Group(), Since: "2026-01-01T00:00:00Z", Approximate: true}
	now := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
	layout := listRowLayout([]prs.PullRequest{local}, 100, now)
	for _, selected := range []bool{false, true} {
		out := layout.render(local, now, selected)
		for _, token := range []string{local.Title, "3d", "checkout*", "topic"} {
			pen := styleAt(t, out, token)
			if pen.Fg != nil || (pen.Bg != nil) != selected {
				t.Fatalf("local metadata invented status color or lost selection: %q %+v", token, pen)
			}
		}
	}
}

func TestRepositoryInlineReferenceAndCurrentMarkers(t *testing.T) {
	unlinked := fixture(123)
	linked := unlinked
	linked.Links = []prs.Link{{WorkspaceID: "workspace", Path: "/work/repo", Label: "Local workspace title"}}
	here := linked
	here.Links = []prs.Link{{WorkspaceID: "workspace", Path: "/work/repo", Current: true}}
	local := localFixture("local", "/work/checkout")
	local.CreatedAt = "2026-01-01T00:00:00Z" // never a GitHub age
	away := local
	away.Links = nil
	for _, tc := range []struct {
		name string
		p    prs.PullRequest
		want string
	}{
		{"unlinked", unlinked, "repo #123"},
		{"linked", linked, "repo #123↗"},
		{"current PR", here, "repo #123↗*"},
		{"current local", local, "checkout*"},
		{"local without link", away, "checkout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := listRowLayout([]prs.PullRequest{tc.p}, 100, time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC))
			out := ansi.Strip(layout.render(tc.p, time.Time{}, false))
			if !strings.Contains(out, tc.want) || !strings.Contains(out, tc.p.Branch) || strings.Contains(out, "Local workspace title") || strings.Contains(out, "unlinked") || strings.Contains(out, "current") || strings.Contains(out, " - ") {
				t.Fatal("dedicated/redundant linkage marker: " + out)
			}
			if tc.p.IsLocal() && (layout.age != 0 || layout.status != 0 || strings.Contains(out, "↗") || strings.Contains(out, "#0")) {
				t.Fatal("local navigation invented GitHub metadata: " + out)
			}
		})
	}
	// Search terms retain semantics even though their words aren't row cells.
	for _, tc := range []struct {
		p     prs.PullRequest
		query string
	}{
		{unlinked, "unlinked"}, {linked, "linked"}, {here, "linked current"}, {local, "linked current"},
	} {
		m := modelWith(t, Actions{}, tc.p)
		typeQuery(m, tc.query)
		if selected, ok := m.selected(); !ok || selected.Key() != tc.p.Key() {
			t.Fatalf("semantic search lost %q for %s", tc.query, tc.p.Key())
		}
	}
}

func TestLongRepositoryPreservesPRReferenceAndArrow(t *testing.T) {
	p := fixture(123)
	p.Repository = "owner/" + strings.Repeat("界👩‍💻e\u0301-repository", 10)
	p.Links = []prs.Link{{WorkspaceID: "workspace", Path: "/work/repo", Current: true}}
	const ref = "#123↗*"
	for _, width := range []int{0, 1, 5, 6, 7, 12, 24} {
		out := rowRepositoryText(p, width)
		if ansi.StringWidth(out) > width || strings.ContainsAny(out, "\n\r") {
			t.Fatalf("repository overflow at %d: %q", width, out)
		}
		if width >= ansi.StringWidth(ref) && !strings.HasSuffix(out, ref) {
			t.Fatalf("width %d lost PR reference/navigation/current: %q", width, out)
		}
	}
	layout := listRowLayout([]prs.PullRequest{p}, 74, time.Time{})
	out := ansi.Strip(layout.render(p, time.Time{}, false))
	if !strings.Contains(out, ref) || !strings.Contains(out, "...") {
		t.Fatal("render did not use reference-preserving repository cell: " + out)
	}
}
