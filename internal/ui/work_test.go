package ui

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/owenps/herdr-prs/internal/prs"
)

func localFixture(id, path string) prs.PullRequest {
	return prs.PullRequest{Local: &prs.LocalWork{ID: id, Path: path}, Title: "Local work", Branch: "topic", Links: []prs.Link{{WorkspaceID: id, Path: path, Current: true}}}
}

func TestLocalPreviewDetailsAndActionsNeverUseGitHub(t *testing.T) {
	p := localFixture("workspace", "/work/checkout")
	p.Links = append(p.Links, prs.Link{WorkspaceID: "agent", PaneID: "agent-pane", Agent: true})
	p.CreatedAt = "2025-01-01T00:00:00Z" // Local creation timestamps are never displayed.
	for _, wide := range []bool{false, true} {
		t.Run(map[bool]string{false: "narrow", true: "wide"}[wide], func(t *testing.T) {
			calls := 0
			var jumped prs.Link
			m := modelWith(t, Actions{
				Detail:       func(context.Context, prs.PullRequest) (prs.Detail, error) { calls++; return prs.Detail{}, nil },
				DetailStream: func(context.Context, prs.PullRequest, bool, func(prs.DetailUpdate)) error { calls++; return nil },
				OpenURL:      func(context.Context, string) error { calls++; return nil },
				Jump:         func(_ context.Context, link prs.Link) error { jumped = link; return nil },
			}, p)
			if wide {
				_, render := m.Update(tea.WindowSizeMsg{Width: 120, Height: 12})
				finishRender(t, m, render)
				if m.detail == nil || m.detailLoading {
					t.Fatal("local preview not immediate")
				}
			}
			if cmd := press(m, tea.KeyCtrlO); cmd != nil {
				t.Fatal("local browser scheduled")
			}
			finishRender(t, m, press(m, tea.KeyCtrlD))
			out := ansi.Strip(strings.Join(m.detailLines(), "\n"))
			if !strings.Contains(out, "No pull request linked yet.") || strings.Contains(out, "Checks") {
				t.Fatal("local detail action did not show local content: " + out)
			}
			if cmd := press(m, tea.KeyCtrlO); cmd != nil {
				t.Fatal("fullscreen local browser scheduled")
			}
			press(m, tea.KeyEsc)
			cmd := press(m, tea.KeyEnter)
			if cmd == nil {
				t.Fatal("local cannot jump")
			}
			m.Update(cmd())
			preferred, _ := p.PreferredLink()
			if jumped != preferred || !m.quitting || calls != 0 {
				t.Fatalf("jump=%+v calls=%d", jumped, calls)
			}
		})
	}
}

func TestLocalSearchRetainsCheckoutSemanticsAndHeaderFallback(t *testing.T) {
	p := localFixture("space", "/notes/project")
	p.Branch = ""
	m := modelWith(t, Actions{}, p)
	typeQuery(m, "in progress linked notes")
	if selected, ok := m.selected(); !ok || selected.Key() != p.Key() || !strings.Contains(plain(m), "project*") {
		t.Fatal("local checkout/group/link semantics not searchable")
	}
	p.Local.Path, p.Repository = "", "owner/project"
	header := detailHeader(p, 80, time.Time{})
	if len(header) != 2 || ansi.Strip(header[1]) != "project" {
		t.Fatal("local header lost repository fallback", header)
	}
}

func TestLocalToPRSelectionFollowsOnlySelectedCheckout(t *testing.T) {
	for _, full := range []bool{false, true} {
		for _, path := range []string{"/work/selected", ""} {
			t.Run(map[bool]string{false: "preview", true: "full"}[full]+path, func(t *testing.T) {
				local := localFixture("selected", path)
				m := wideModel(t, Actions{}, local, fixture(1))
				if full {
					finishRender(t, m, press(m, tea.KeyCtrlD))
				}
				other, target := fixture(2), fixture(3)
				other.Unresolved = 1
				other.Links = []prs.Link{{WorkspaceID: "selected", Path: "/work/other"}}
				target.Links = []prs.Link{{WorkspaceID: "selected", Path: path}}
				// Workspace-only identities must be unambiguous.
				if path == "" {
					other.Links[0].WorkspaceID = "other"
				}
				m.Update(catalogUpdateMsg{token: m.loadToken, update: prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{other, target, fixture(1)}}}})
				if chosen(m) != target.Number || m.detailKey != target.Key() || m.detail != nil {
					t.Fatal("transition lost selection or leaked local preview")
				}
			})
		}
	}
	local := localFixture("selected", "/work/selected")
	wrong := fixture(2)
	wrong.Links = []prs.Link{{WorkspaceID: "selected", Path: "/work/other"}}
	if _, ok := linkedReplacement(local, []prs.PullRequest{wrong}); ok {
		t.Fatal("followed other checkout")
	}
	a, b := fixture(3), fixture(4)
	a.Links, b.Links = local.Links, local.Links
	if _, ok := linkedReplacement(local, []prs.PullRequest{a, b}); ok {
		t.Fatal("ambiguous checkout guessed")
	}
	// Explicit movement away from local must not get pulled back by discovery.
	m := wideModel(t, Actions{}, local, fixture(1))
	press(m, tea.KeyUp) // In Review precedes the initially selected local group.
	m.Update(catalogUpdateMsg{token: m.loadToken, update: prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{a, fixture(1)}}}})
	if chosen(m) != 1 {
		t.Fatal("explicit selection overridden")
	}
}

func TestClockLifecycleStartsAndStopsWithPopup(t *testing.T) {
	created := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	// Startup advances the clock regardless of command batching or order.
	startup := newStreamHarness(t, Actions{Load: func(context.Context) (prs.Catalog, error) {
		return prs.Catalog{}, nil
	}})
	startup.m.clock = created
	startup.start(startup.m.Init())
	startup.until(t, func() bool { return startup.m.clock.After(created) })
	pending := startup.m.tick()
	startup.deliver(tea.KeyMsg{Type: tea.KeyCtrlC})
	if pending() != nil {
		t.Fatal("cancelled tick delivered")
	}
	if _, next := startup.m.Update(clockMsg(created)); next != nil {
		t.Fatal("quit ticker restarted")
	}
	m := modelWith(t, Actions{}, fixture(1))
	pending = m.tick()
	press(m, tea.KeyEsc) // List quit.
	if pending() != nil {
		t.Fatal("Esc did not cancel ticker")
	}
}

func TestPRToLocalToPRKeepsSelectedCheckout(t *testing.T) {
	a, b := fixture(1), fixture(2)
	a.Links = []prs.Link{{WorkspaceID: "same", Path: "/work/a"}}
	b.Links = []prs.Link{{WorkspaceID: "same", Path: "/work/b"}}
	m := wideModel(t, Actions{}, a, b)
	press(m, tea.KeyDown)
	localA, localB := localFixture("a", "/work/a"), localFixture("b", "/work/b")
	m.Update(catalogUpdateMsg{token: m.loadToken, update: prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{localA, localB}}}})
	if p, ok := m.selected(); !ok || p.Key() != localB.Key() {
		t.Fatal("selected another checkout during provisional publication", p)
	}
	m.Update(catalogUpdateMsg{token: m.loadToken, update: prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{a, b}}}})
	if p, ok := m.selected(); !ok || p.Key() != b.Key() {
		t.Fatal("lost original PR after refresh", p)
	}
	wrong := localFixture("wrong", "/work/other")
	wrong.Links[0].WorkspaceID = "same"
	if _, ok := linkedReplacement(b, []prs.PullRequest{wrong}); ok {
		t.Fatal("followed workspace instead of checkout")
	}
}

func TestReviewPresentationKeepsLatestSubmissionPerKnownLogin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reviews []prs.Review
		visible []string
		hidden  []string
	}{
		{"timestamped", []prs.Review{
			{Author: "Alice", State: "APPROVED", Body: "old approval", SubmittedAt: "2025-01-01T00:00:00Z"},
			{Author: "Bob", State: "CHANGES_REQUESTED", Body: "bob latest", SubmittedAt: "2025-01-04T00:00:00Z"},
			{Author: " ALICE ", State: "COMMENTED", Body: "latest comment", SubmittedAt: "2025-01-03T00:00:00Z"},
			{Author: "bob", State: "APPROVED", Body: "bob old", SubmittedAt: "2025-01-02T00:00:00Z"},
			{Body: "deleted one"}, {Body: "deleted two"}, {Author: " ", Body: "unknown"},
		}, []string{"bob latest", "latest comment", "deleted one", "deleted two", "unknown", "COMMENTED"}, []string{"old approval", "bob old"}},
		{"legacy", []prs.Review{
			{Author: "alice", State: "APPROVED", Body: "legacy approval"},
			{Author: "Bob", Body: "independent reviewer"},
			{Author: "ALICE", State: "COMMENTED", Body: "legacy comment"},
		}, []string{"legacy comment", "independent reviewer", "COMMENTED"}, []string{"legacy approval"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(tc.reviews)
			out := ansi.Strip(strings.Join(formatDetailContent(prs.Detail{PR: fixture(1), Reviews: tc.reviews}, 120, func(s string) []string { return []string{s} }), "\n"))
			for _, body := range tc.visible {
				if strings.Count(out, body) != 1 {
					t.Fatalf("latest/anonymous review missing or duplicated: %q in %q", body, out)
				}
			}
			for _, body := range tc.hidden {
				if strings.Contains(out, body) {
					t.Fatalf("superseded review displayed: %q in %q", body, out)
				}
			}
			if !reflect.DeepEqual(tc.reviews, before) {
				t.Fatal("review reduction mutated source data")
			}
		})
	}
}
