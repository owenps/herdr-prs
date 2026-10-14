package ui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/owenps/herdr-prs/internal/prs"
)

// Inspect the rendered pane, not an assumed items-per-page formula. The same
// assertion works at one cell and in split mode; metadata must fit when possible.
func assertListSelection(t *testing.T, m *Model) {
	t.Helper()
	p, ok := m.selected()
	if !ok {
		t.Fatal("missing selected item")
	}
	width := m.width
	if m.split() {
		width, _ = m.paneWidths()
	}
	body := m.paneBody(width, false)
	columns := listRowLayout(m.rows, width, m.clock)
	want := ansi.Strip(columns.render(p, m.clock, true))
	i := 0
	if m.contentHeight() >= 2 {
		i = 1 + m.cursor - m.top
		for row := m.top + 1; row <= m.cursor; row++ {
			if m.rows[row].Group() != m.rows[row-1].Group() {
				i++
			}
		}
	}
	if i >= 0 && i < len(body) && ansi.Strip(body[i]) == want {
		if width >= 80 && !strings.Contains(ansi.Strip(body[i]), rowStatus(p)) {
			t.Fatalf("selected row missing status: %q", body[i])
		}
		if m.contentHeight() >= 2 {
			heading := fmt.Sprintf("%s (%d)", p.Group(), groupCount(m.rows, p.Group()))
			found := false
			for _, prior := range body[:i] {
				found = found || ansi.Strip(prior) == expectedGroupHeading(heading, width)
			}
			if !found {
				t.Fatalf("selected context heading hidden: %q", body)
			}
		}
		return
	}
	t.Fatalf("selected title hidden: top=%d cursor=%d body=%q", m.top, m.cursor, body)
}

// Independent plaintext expectation: exact count/label followed by a pane-filling rule.
func expectedGroupHeading(label string, width int) string {
	if width <= 0 {
		return ""
	}
	label = fit(label, width)
	if remaining := width - ansi.StringWidth(label); remaining > 0 {
		label += " " + strings.Repeat("─", remaining-1)
	}
	return label
}

func groupCount(rows []prs.PullRequest, group prs.Group) int {
	count := 0
	for _, p := range rows {
		if p.Group() == group {
			count++
		}
	}
	return count
}

func mixedGroups() []prs.PullRequest {
	ps := make([]prs.PullRequest, 12)
	for i := range ps {
		ps[i] = fixture(i + 1)
	}
	ps[0].State, ps[1].State = "MERGED", "MERGED"
	ps[2].Unresolved = 1
	ps[3].MergeState = "DIRTY"
	ps[4].Checks = []prs.Check{{Conclusion: "FAILURE"}}
	ps[5].MergeState = "CLEAN"
	ps[6].Checks = []prs.Check{{Status: "IN_PROGRESS"}}
	ps[8].State = ""
	ps[9].Draft, ps[9].Unresolved = true, 3
	ps[10] = localFixture("local", "/work/local")
	ps[10].Links = nil
	ps[11].State = "CLOSED"
	return ps
}

func TestGroupedSectionsShowOnlyNonemptyGroupsAndTheirFilteredCounts(t *testing.T) {
	ps := mixedGroups()
	slices.Reverse(ps)
	m := modelWith(t, Actions{}, ps...)
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 50})
	body := strings.Join(m.paneBody(90, false), "\n")
	plainBody := ansi.Strip(body)
	previous := -1
	for _, heading := range []string{"Done (2)", "In Review (7)", "In Progress (2)", "Cancelled (1)"} {
		i := strings.Index(plainBody, heading)
		if i <= previous || strings.Count(plainBody, heading) != 1 || !slices.Contains(strings.Split(plainBody, "\n"), expectedGroupHeading(heading, 90)) {
			t.Fatalf("section order/count: %q", plainBody)
		}
		previous = i
	}
	previous = -1
	for _, title := range []string{"Change 1", "Change 2", "Change 3", "Change 5", "Change 4", "Change 6", "Change 7", "Change 8", "Change 9", "Change 10", "Local work", "Change 12"} {
		index := strings.Index(plainBody, title+" ")
		if index <= previous {
			t.Fatalf("group/priority order lost visible row %q: %s", title, plainBody)
		}
		previous = index
	}

	// Empty groups never produce headings; list grouping never enters details.
	m = modelWith(t, Actions{}, fixture(20))
	body = ansi.Strip(strings.Join(m.paneBody(90, false), "\n"))
	if !strings.HasPrefix(body, expectedGroupHeading("In Review (1)", 90)+"\n› ") || strings.Contains(body, "Done (") || strings.Contains(body, "In Progress (") || strings.Contains(body, "Cancelled (") {
		t.Fatal(body)
	}
	m.actions.Detail = func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		return prs.Detail{PR: p, Body: "# Markdown heading"}, nil
	}
	_, render := m.Update(press(m, tea.KeyCtrlD)())
	finishRender(t, m, render)
	if out := strings.Join(m.paneBody(90, true), "\n"); strings.Contains(out, "In Review (") || !strings.HasPrefix(ansi.Strip(out), fixture(20).Title) {
		t.Fatal("list heading leaked into fullscreen details: " + out)
	}
}

func TestGroupedSearchOrderScoresAndFilteredCounts(t *testing.T) {
	ps := mixedGroups()
	for i := range ps {
		ps[i].Title = "hidden"
	}
	ps[0].Title = "z---z---n---e---e---d---l---e---z---z" // Done beats the tighter active match.
	ps[2].Title = "z--z--n--e--e--d--l--e--z--z"          // Attention loses to tighter match within its group.
	ps[7].Title = "zzneedlezz"
	ps[9].Title = "zzneedlezz"
	ps[11].Title = "zzneedlezz"
	m := modelWith(t, Actions{}, ps...)
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 40})
	typeQuery(m, "zzneedlezz")
	want := []int{1, 8, 3, 10, 12}
	for i, n := range want {
		if len(m.rows) != len(want) || m.rows[i].Number != n {
			t.Fatalf("fuzzy search must stay grouped: %+v", m.rows)
		}
	}
	body := ansi.Strip(strings.Join(m.paneBody(90, false), "\n"))
	for _, heading := range []string{"Done (1)", "In Review (2)", "In Progress (1)", "Cancelled (1)"} {
		if !slices.Contains(strings.Split(body, "\n"), expectedGroupHeading(heading, 90)) {
			t.Fatalf("counts include filtered-out members: %s", body)
		}
	}
	// Group names are search terms, not selectable heading records.
	for _, query := range []string{"In Review", "In Progress", "Done", "Cancelled"} {
		m := modelWith(t, Actions{}, ps...)
		typeQuery(m, query)
		if !strings.Contains(plain(m), query+" (") {
			t.Fatalf("group name query lost its matching section: %q in %q", query, plain(m))
		}
	}
}

func TestGroupedNavigationResizeAndPagingBudgets(t *testing.T) {
	m := modelWith(t, Actions{}, mixedGroups()...)
	for _, width := range []int{1, 80, 120} {
		for _, height := range []int{1, 5, 6, 7, 9, 12, 30, 100} {
			m.Update(tea.WindowSizeMsg{Width: width, Height: height})
			for _, key := range []tea.KeyType{tea.KeyHome, tea.KeyDown, tea.KeyDown, tea.KeyPgDown, tea.KeyEnd, tea.KeyUp, tea.KeyPgUp, tea.KeyHome} {
				press(m, key)
				assertDimensions(t, m)
				assertListSelection(t, m)
			}
			// A trailing heading must always have an item title after it.
			body := m.paneBody(width, false)
			for i, line := range body {
				for _, group := range []prs.Group{prs.GroupDone, prs.GroupInReview, prs.GroupInProgress, prs.GroupCancelled} {
					if ansi.Strip(line) == expectedGroupHeading(fmt.Sprintf("%s (%d)", group, groupCount(m.rows, group)), width) && (i+1 == len(body) || ansi.Strip(body[i+1]) == "") {
						t.Fatal("orphan heading")
					}
				}
			}
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 9}) // Four body lines: heading + three item rows.
	press(m, tea.KeyHome)
	for i := 0; i < len(m.rows); i++ {
		if m.cursor != i {
			t.Fatal("heading became selectable")
		}
		assertListSelection(t, m)
		press(m, tea.KeyDown)
	}
	for i := len(m.rows) - 1; i >= 0; i-- {
		if m.cursor != i {
			t.Fatal("up skipped a PR or selected a heading")
		}
		press(m, tea.KeyUp)
	}
}

func TestGroupedResizeBackfillsWholeTail(t *testing.T) {
	for _, count := range []int{12, 120} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			ps := make([]prs.PullRequest, count)
			for i := range ps {
				ps[i] = fixture(i + 1)
				if i < count/2 {
					ps[i].State = "MERGED"
				}
				if i >= 3*count/4 {
					ps[i].Draft = true
				}
			}
			m := modelWith(t, Actions{}, ps...)
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 100})
			press(m, tea.KeyEnd)
			selected, _ := m.selected()
			for _, tiny := range []int{1, 2, 3, 7} {
				m.Update(tea.WindowSizeMsg{Width: 80, Height: tiny})
				if m.top != len(m.rows)-1 {
					t.Fatalf("tiny body did not select tail: top=%d", m.top)
				}
				assertListSelection(t, m)
				m.Update(tea.WindowSizeMsg{Width: 80, Height: 100})
				p, _ := m.selected()
				if p.Key() != selected.Key() || m.cursor != len(m.rows)-1 {
					t.Fatal("resize changed selection")
				}
				layout, height := groupedList(m.rows), m.contentHeight()
				if layout.height(m.top, m.cursor) > height || m.top > 0 && layout.height(m.top-1, m.cursor) <= height {
					t.Fatalf("tail not maximally backfilled: top=%d height=%d", m.top, height)
				}
				if count == 12 && m.top != 0 || count == 120 && m.top == 0 {
					t.Fatalf("wrong tail start: count=%d top=%d", count, m.top)
				}
				assertListSelection(t, m)
				blocks := layout.window(m.top, height)
				if blocks[len(blocks)-1].item != m.cursor || blocks[len(blocks)-1].lines != 1 {
					t.Fatal("expanded viewport lost selected row")
				}
				body := m.paneBody(80, false)
				for i, line := range body {
					for _, group := range []prs.Group{prs.GroupDone, prs.GroupInReview, prs.GroupInProgress} {
						if ansi.Strip(line) == expectedGroupHeading(fmt.Sprintf("%s (%d)", group, groupCount(m.rows, group)), 80) && (i+1 == len(body) || ansi.Strip(body[i+1]) == "") {
							t.Fatal("expanded viewport has orphan heading")
						}
					}
				}
			}
		})
	}
}

func TestGroupedInitialCurrentOffscreenAndContextCounts(t *testing.T) {
	var ps []prs.PullRequest
	for i := 1; i <= 50; i++ {
		p := fixture(i)
		p.Title = fmt.Sprintf("Title %03d", i)
		ps = append(ps, p)
	}
	ps[35].Links = []prs.Link{{Current: true}}
	// A current draft lives after all review rows, regardless of priority.
	ps[35].Draft = true
	m := NewModel(context.Background(), Actions{})
	t.Cleanup(m.cancel)
	m.width, m.height = 80, 10
	m.applyCatalog(prs.CatalogUpdate{Catalog: prs.Catalog{PRs: ps}})
	if chosen(m) != 36 || m.cursor != len(m.rows)-1 || m.top == 0 {
		t.Fatal("initial current selection lost to group order")
	}
	assertListSelection(t, m)
	press(m, tea.KeyHome)
	for range 20 {
		press(m, tea.KeyDown)
	}
	body := ansi.Strip(strings.Join(m.paneBody(80, false), "\n"))
	if !strings.HasPrefix(body, expectedGroupHeading("In Review (49)", 80)+"\n") {
		t.Fatal("viewport lost full filtered group context: " + body)
	}
}

func TestDetailGroupMigrationPreservesIdentityScrollAndFilteredCounts(t *testing.T) {
	p := fixture(20)
	p.Title = "zzneedlezz selected"
	p.GroupTiming = &prs.GroupTiming{Group: p.Group(), Since: "2026-01-01T00:00:00Z"}
	ps := append(mixedGroups(), p)
	m := wideModel(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		return prs.Detail{PR: p, Body: strings.Repeat("paragraph\n\n", 50)}, nil
	}}, ps...)
	m.clock = time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
	typeQuery(m, "zzneedlezz")
	_, render := m.Update(previewRequest(t, m)())
	finishRender(t, m, render)
	press(m, tea.KeyTab)
	press(m, tea.KeyPgDown)
	press(m, tea.KeyTab)
	scroll, token := m.detailTop, m.detailToken
	if scroll == 0 {
		t.Fatal("test did not scroll detail")
	}
	key, epoch, focus := m.renderKey(m.detailWidth()), m.renderEpoch, m.previewFocused
	content := slices.Clone(m.renderedDetails[key])
	for i, transition := range []struct {
		state string
		draft bool
		group prs.Group
	}{{"MERGED", false, prs.GroupDone}, {"CLOSED", false, prs.GroupCancelled}, {"OPEN", true, prs.GroupInProgress}, {"OPEN", false, prs.GroupInReview}} {
		fresh := *m.detail
		fresh.PR.State, fresh.PR.Draft = transition.state, transition.draft
		// An old stage's clock must disappear immediately, not look like the
		// new stage has existed since PR creation.
		if groupAge(fresh.PR, m.clock) != "" {
			t.Fatal("old group clock survived transition")
		}
		fresh.PR.GroupTiming = &prs.GroupTiming{Group: transition.group, Since: m.clock.Add(-time.Duration(10+i) * time.Second).Format(time.RFC3339)}
		_, render = m.Update(detailUpdateMsg{token: token, key: p.Key(), update: prs.DetailUpdate{Detail: fresh}})
		if render != nil || m.renderEpoch != epoch || m.renderKey(m.detailWidth()) != key || !slices.Equal(content, m.renderedDetails[key]) || m.previewFocused != focus {
			t.Fatal("clock/group transition reparsed prose or lost focus")
		}
		selected, _ := m.selected()
		if selected.Key() != p.Key() || selected.Group() != transition.group || m.detailKey != p.Key() || m.detail.PR.Key() != p.Key() || m.detailTop != scroll || m.detailToken != token {
			t.Fatalf("migration lost selection/detail/scroll: selected=%+v scroll=%d token=%d", selected, m.detailTop, m.detailToken)
		}
		if got := groupAge(selected, m.clock); got != fmt.Sprintf("%ds", 10+i) {
			t.Fatal("group transition did not reset clock", got)
		}
		assertListSelection(t, m)
		body := ansi.Strip(strings.Join(m.paneBody(80, false), "\n"))
		if !strings.HasPrefix(body, expectedGroupHeading(fmt.Sprintf("%s (1)", transition.group), 80)+"\n") || len(m.rows) != 1 {
			t.Fatal("filtered counts or empty group headings regressed: " + body)
		}
		// Cached prose cannot undo the authoritative group transition.
		cached := fresh
		cached.PR = p
		m.Update(detailUpdateMsg{token: token, key: p.Key(), update: prs.DetailUpdate{Detail: cached, Cached: true}})
		if m.rows[0].Group() != transition.group || m.rows[0].GroupTiming == nil || *m.rows[0].GroupTiming != *fresh.PR.GroupTiming {
			t.Fatal("cached status/clock undid live group")
		}
	}
	// Clearing the filter reranks the full catalog, and later catalog snapshots
	// preserve the selected key even when it migrates back across all groups.
	m.query = ""
	m.rank(p.Key(), false)
	p.State = "MERGED"
	ps[len(ps)-1] = p
	m.applyCatalog(prs.CatalogUpdate{Catalog: prs.Catalog{PRs: ps}})
	if chosen(m) != p.Number {
		t.Fatal("catalog regroup lost selected identity")
	}
	assertListSelection(t, m)
}

func TestLiveGroupMigrationKeepsFullListContiguous(t *testing.T) {
	m := wideModel(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		p.State = "MERGED"
		return prs.Detail{PR: p, Body: "same PR detail"}, nil
	}}, mixedGroups()...)
	// Select a middle review item, not the first member of any section.
	m.rank(fixture(8).Key(), false)
	m.syncPreview(false)
	_, render := m.Update(previewRequest(t, m)())
	finishRender(t, m, render)
	if chosen(m) != 8 || m.detail.PR.Number != 8 || m.detail.PR.Group() != prs.GroupDone {
		t.Fatal("live migration lost selected PR")
	}
	for i := 1; i < len(m.rows); i++ {
		if m.rows[i-1].Group().Order() > m.rows[i].Group().Order() {
			t.Fatalf("detail left a split group: %+v", m.rows)
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 50})
	body := ansi.Strip(strings.Join(m.paneBody(90, false), "\n"))
	if strings.Count(body, "Done (3)") != 1 || strings.Count(body, "In Review (6)") != 1 {
		t.Fatal("migration headings/counts: " + body)
	}
	assertListSelection(t, m)
}
