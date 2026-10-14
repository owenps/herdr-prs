package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

func groupFixture(number int, group prs.Group) prs.PullRequest {
	p := fixture(number)
	switch group {
	case prs.GroupDone:
		p.State = "MERGED"
	case prs.GroupInProgress:
		p.Draft = true
	case prs.GroupCancelled:
		p.State = "CLOSED"
	}
	return p
}

func TestCtrlDFullToggleReflowsWithoutRefetchOrScrollLoss(t *testing.T) {
	for _, width := range []int{80, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			calls := 0
			p := fixture(1)
			m := modelWith(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
				calls++
				return prs.Detail{PR: p, Body: "# Cached prose\n\n" + strings.Repeat("a long paragraph with words that must reflow across widths\n\n", 40)}, nil
			}}, p)
			m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
			if m.split() {
				_, render := m.Update(previewRequest(t, m)())
				finishRender(t, m, render)
				press(m, tea.KeyTab)
				press(m, tea.KeyDown)
				press(m, tea.KeyDown)
				before, token := m.detail, m.detailToken
				finishRender(t, m, press(m, tea.KeyCtrlD))
				if m.detail != before || m.detailToken != token || m.detailTop != 2 {
					t.Fatal("entering fullscreen replaced accepted detail/request/scroll")
				}
			} else {
				_, render := m.Update(press(m, tea.KeyCtrlD)())
				finishRender(t, m, render)
			}
			if !m.inDetail || calls != 1 || m.renderKey(m.detailWidth()).width != width {
				t.Fatal("fullscreen did not use one fetch and its own width")
			}
			press(m, tea.KeyHome)
			if !strings.Contains(plain(m), "Cached prose") {
				t.Fatal("fullscreen lost cached prose")
			}
			press(m, tea.KeyPgDown)
			scroll, body := m.detailTop, m.detail.Body
			finishRender(t, m, press(m, tea.KeyCtrlD))
			if m.inDetail || calls != 1 || m.previewFocused != (width >= 100) || m.detailTop != scroll || m.detail.Body != body {
				t.Fatal("second Ctrl-D retried or lost content/scroll/focus")
			}
			if m.split() {
				press(m, tea.KeyDown)
				if m.detailTop != scroll+1 || chosen(m) != 1 {
					t.Fatal("wide return did not retain scrolling focus")
				}
				press(m, tea.KeyTab)
				if m.previewFocused || !strings.HasPrefix(m.footer(), "List ·") {
					t.Fatal("Tab did not return list focus")
				}
			}
			finishRender(t, m, press(m, tea.KeyCtrlD))
			if calls != 1 || !m.inDetail {
				t.Fatal("reopening refetched cached detail")
			}
			press(m, tea.KeyEsc)
			if m.inDetail || m.previewFocused || m.quitting {
				t.Fatal("Escape changed its existing back behavior")
			}
			assertDimensions(t, m)
		})
	}
}

func TestFullscreenFetchesChangedRevisionInsteadOfOldCache(t *testing.T) {
	calls := 0
	m := modelWith(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		calls++
		return prs.Detail{PR: p, Body: p.UpdatedAt}, nil
	}}, fixture(1))
	_, render := m.Update(press(m, tea.KeyCtrlD)())
	finishRender(t, m, render)
	press(m, tea.KeyCtrlD)
	fresh := cloneCatalog(m.catalog)
	fresh.PRs[0].UpdatedAt = "NEW REVISION"
	m.applyCatalog(prs.CatalogUpdate{Catalog: fresh})
	fetch := press(m, tea.KeyCtrlD)
	if fetch == nil || !m.detailLoading || calls != 1 {
		t.Fatal("changed revision reused obsolete cached prose")
	}
	_, render = m.Update(fetch())
	finishRender(t, m, render)
	if calls != 2 || m.detail.Body != "NEW REVISION" || m.detailVersion != version(fresh.PRs[0]) {
		t.Fatal("changed revision did not fetch new details")
	}
}

func TestFullscreenEntryPreservesExplicitRefreshForce(t *testing.T) {
	forces := make(chan bool, 1)
	m := modelWith(t, Actions{DetailStream: func(_ context.Context, _ prs.PullRequest, force bool, _ func(prs.DetailUpdate)) error {
		forces <- force
		return nil
	}}, fixture(1))
	m.Update(press(m, tea.KeyCtrlR)())
	if m.detailKey != "" || !m.forceDetail {
		t.Fatal("narrow refresh fixture did not require forced first detail")
	}
	m.Update(press(m, tea.KeyCtrlD)())
	requireFlag(t, forces, true)
}

func TestFullscreenErrorsAdvertiseCtrlRAndCtrlDOnlyCloses(t *testing.T) {
	calls := 0
	m := modelWith(t, Actions{Detail: func(context.Context, prs.PullRequest) (prs.Detail, error) {
		calls++
		return prs.Detail{}, errors.New("denied")
	}}, fixture(1))
	m.Update(press(m, tea.KeyCtrlD)())
	view := plain(m)
	if !strings.Contains(view, "Ctrl-R to retry") || !strings.Contains(view, "^R refresh") || strings.Contains(view, "Ctrl-D to retry") || strings.Contains(view, "^D retry") {
		t.Fatal("error does not identify refresh/retry separately from navigation")
	}
	if press(m, tea.KeyCtrlD) != nil || m.inDetail || calls != 1 {
		t.Fatal("Ctrl-D retried failed request")
	}
	press(m, tea.KeyCtrlD) // Missing detail is needed on entry, not on exit.
	m.actions.Detail = func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		calls++
		return prs.Detail{PR: p, Body: "retried prose"}, nil
	}
	_, fetch := m.Update(press(m, tea.KeyCtrlR)())
	_, render := m.Update(fetch())
	finishRender(t, m, render)
	if calls != 2 || m.detailErr != "" || m.detail.Body != "retried prose" {
		t.Fatal("Ctrl-R did not retry successfully")
	}
}

func TestFullscreenReusesInflightDetailAndPromotesDebounce(t *testing.T) {
	p := fixture(1)
	calls := 0
	m := wideModel(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		calls++
		return prs.Detail{PR: p, Body: "inflight prose"}, nil
	}}, p)
	debounce := previewMsg{token: m.detailToken, key: p.Key()}
	fetch := press(m, tea.KeyCtrlD)
	if fetch == nil || !m.inDetail || m.detailDebouncing || !m.detailLoading {
		t.Fatal("fullscreen did not promote debounce")
	}
	if _, cmd := m.Update(debounce); cmd != nil {
		t.Fatal("superseded debounce fetched")
	}
	token := m.detailToken
	if press(m, tea.KeyCtrlD) != nil || !m.previewFocused || !m.detailLoading || m.detailToken != token {
		t.Fatal("wide close replaced inflight request")
	}
	if press(m, tea.KeyCtrlD) != nil || !m.inDetail || m.detailToken != token {
		t.Fatal("fullscreen entry duplicated inflight request")
	}
	_, render := m.Update(fetch())
	finishRender(t, m, render)
	if calls != 1 || m.detail.Body != "inflight prose" {
		t.Fatal("inflight result was not accepted")
	}
}

func TestNarrowFullscreenToggleCancelsAndRejectsLateSameKey(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	results := make(chan tea.Msg, 1)
	p := fixture(1)
	m := modelWith(t, Actions{Detail: func(ctx context.Context, p prs.PullRequest) (prs.Detail, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return prs.Detail{PR: p, Body: "LATE PROSE"}, nil
	}}, p)
	fetch := press(m, tea.KeyCtrlD)
	oldToken := m.detailToken
	go func() { results <- fetch() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	if press(m, tea.KeyCtrlD) != nil || m.inDetail || m.detailLoading || m.detailToken == oldToken {
		t.Fatal("narrow close failed to invalidate request")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("narrow close did not cancel")
	}
	m.actions.Detail = func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		return prs.Detail{PR: p, Body: "NEW PROSE"}, nil
	}
	fresh := press(m, tea.KeyCtrlD)
	m.Update(<-results)
	m.Update(detailMsg{token: oldToken, key: p.Key(), err: errors.New("late error")})
	if m.detail != nil || !m.detailLoading || m.detailErr != "" {
		t.Fatal("same-key late result defeated cancellation generation")
	}
	_, render := m.Update(fresh())
	finishRender(t, m, render)
	if m.detail.Body != "NEW PROSE" {
		t.Fatal("new needed fetch did not work")
	}
}

func TestGroupFilterCyclesEmptyGroupsWithoutFetchOrQueryExpansion(t *testing.T) {
	calls := 0
	m := modelWith(t, Actions{
		Load: func(context.Context) (prs.Catalog, error) {
			calls++
			return prs.Catalog{PRs: []prs.PullRequest{fixture(1)}}, nil
		},
		Detail: func(context.Context, prs.PullRequest) (prs.Detail, error) {
			calls++
			return prs.Detail{}, nil
		},
	})
	typeQuery(m, "Change")
	for round := 0; round < 2; round++ {
		for _, group := range []prs.Group{prs.GroupDone, prs.GroupInReview, prs.GroupInProgress, prs.GroupCancelled, ""} {
			if press(m, tea.KeyShiftTab) != nil || m.groupFilter != group || m.query != "Change" || calls != 1 {
				t.Fatalf("filter cycle fetched or changed query/group: %q %q calls=%d", m.groupFilter, m.query, calls)
			}
			empty := group != "" && group != prs.GroupInReview
			if empty != (len(m.rows) == 0) || empty && !strings.Contains(plain(m), "No matching items") {
				t.Fatal("empty group skipped or body unstable")
			}
			assertDimensions(t, m)
		}
	}
	if !strings.Contains(m.footer(), "⇧Tab groups") || !strings.HasSuffix(ansi.Strip(m.searchHeader()), "All") {
		t.Fatal("missing global group controls")
	}
}

func TestGroupFilterSelectionQueryRankingCountsAndRefresh(t *testing.T) {
	done := groupFixture(1, prs.GroupDone)
	done.Title = "fast" // Strongest fuzzy match is outside the selected group.
	a, b := fixture(2), fixture(3)
	b.Title = "Change fast"
	progress := groupFixture(4, prs.GroupInProgress)
	cancelled := groupFixture(5, prs.GroupCancelled)
	m := modelWith(t, Actions{}, done, a, b, progress, cancelled)
	press(m, tea.KeyDown)
	press(m, tea.KeyDown)
	if chosen(m) != b.Number {
		t.Fatal("fixture did not select second review")
	}
	// Filter first, then fuzzy score within the selected group. It is not an
	// extra search term: a Done PR with an exact title cannot enter In Review.
	press(m, tea.KeyShiftTab)
	if chosen(m) != done.Number {
		t.Fatal("invisible selection did not choose first match")
	}
	press(m, tea.KeyShiftTab)
	press(m, tea.KeyDown)
	if chosen(m) != b.Number || m.groupFilter != prs.GroupInReview {
		t.Fatal("review filter did not expose both matches")
	}
	m.Update(press(m, tea.KeyCtrlR)())
	if chosen(m) != b.Number || m.groupFilter != prs.GroupInReview {
		t.Fatal("catalog refresh lost filter or selected identity")
	}
	typeQuery(m, "fast")
	if len(m.rows) != 1 || chosen(m) != b.Number || !strings.Contains(plain(m), "In Review (1)") || !strings.Contains(plain(m), "1/5 items") {
		t.Fatal("visible counts/query ranking do not respect filter")
	}
	m.Update(press(m, tea.KeyCtrlR)())
	if m.query != "fast" || m.groupFilter != prs.GroupInReview || chosen(m) != b.Number {
		t.Fatal("refresh reset query/group/selection")
	}
	for range 5 {
		press(m, tea.KeyShiftTab)
	}
	if m.query != "fast" || m.groupFilter != prs.GroupInReview || chosen(m) != b.Number {
		t.Fatal("cycle lost query or visible selection")
	}
	for range len("fast") {
		press(m, tea.KeyBackspace)
	}
	typeQuery(m, "Change")
	if got := []int{m.rows[0].Number, m.rows[1].Number}; !slices.Equal(got, []int{a.Number, b.Number}) {
		t.Fatal("within-group score/tie ordering changed")
	}
}

func TestGroupFilterPreservesStillVisibleSelectedIdentity(t *testing.T) {
	first, second := groupFixture(1, prs.GroupDone), groupFixture(6, prs.GroupDone)
	m := modelWith(t, Actions{}, first, second, fixture(2))
	press(m, tea.KeyDown)
	if chosen(m) != second.Number {
		t.Fatal("fixture did not select second Done item")
	}
	press(m, tea.KeyShiftTab)
	if chosen(m) != second.Number || !strings.Contains(plain(m), "Done (2)") || !strings.Contains(plain(m), "2/3 items") {
		t.Fatal("filter moved still-visible selection or lost group counts")
	}
}

func TestGroupFilteringFullscreenClearsWrongPinnedDataAndKeepsQuery(t *testing.T) {
	for _, width := range []int{80, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			calls := 0
			m := modelWith(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
				calls++
				return prs.Detail{PR: p, Body: "OLD PINNED DATA"}, nil
			}}, fixture(1))
			m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
			typeQuery(m, "Change")
			_, render := m.Update(press(m, tea.KeyCtrlD)())
			finishRender(t, m, render)
			token := m.detailToken
			if press(m, tea.KeyShiftTab) != nil || m.inDetail || m.previewFocused || m.groupFilter != prs.GroupDone || m.query != "Change" || len(m.rows) != 0 || calls != 1 {
				t.Fatal("full filter did not return to empty normal view")
			}
			if strings.Contains(plain(m), "OLD PINNED DATA") || m.split() && (m.detail != nil || m.detailKey != "") {
				t.Fatal("wrong pinned detail remained after filtering")
			}
			m.Update(detailMsg{token: token, key: fixture(1).Key(), detail: prs.Detail{PR: fixture(1), Body: "LATE WRONG DATA"}})
			if strings.Contains(plain(m), "LATE WRONG DATA") {
				t.Fatal("late pinned data shown in filtered view")
			}
			press(m, tea.KeyShiftTab)
			if chosen(m) != 1 || m.inDetail || calls != 1 {
				t.Fatal("review selection/cache not restored locally")
			}
		})
	}
}

func TestSearchHeaderFitsQueryAndGroupFilter(t *testing.T) {
	m := modelWith(t, Actions{}, fixture(1))
	for _, profile := range []termenv.Profile{termenv.Ascii, termenv.TrueColor} {
		m.markdownProfile = profile
		for width := 1; width <= 200; width++ {
			m.width = width
			for _, group := range []prs.Group{"", prs.GroupDone, prs.GroupInReview, prs.GroupInProgress, prs.GroupCancelled} {
				m.groupFilter = group
				label := string(group)
				if group == "" {
					label = "All"
				}
				for _, query := range []string{"", "typed", strings.Repeat("界👩‍💻e\u0301", 45) + "END", "x\x1b[31m界\x1b[0m\x1b]52;c;payload\a\u202eEND"} {
					m.query = query
					header := m.searchHeader()
					if ansi.StringWidth(header) > width || strings.Contains(header, "payload") || strings.Contains(header, "\u202e") {
						t.Fatalf("width=%d query=%q: unsafe/overflowing header %q", width, query, header)
					}
					if profile == termenv.Ascii && strings.Count(header, "|") != 1 || profile != termenv.Ascii && strings.Count(header, "\x1b[97;7m \x1b[0m") != 1 {
						t.Fatal("search caret hidden or duplicated")
					}
					out := ansi.Strip(header)
					if width >= 12+2+ansi.StringWidth(label) {
						if !strings.HasSuffix(out, "  "+label) || ansi.StringWidth(out) != width {
							t.Fatalf("filter label not right aligned at %d: %q", width, out)
						}
						end := "END"
						if profile == termenv.Ascii {
							end += "|"
						}
						if strings.HasSuffix(query, "END") && !strings.HasSuffix(strings.TrimRight(strings.TrimSuffix(out, label), " "), end) {
							t.Fatal("filter reservation lost query end")
						}
					} else if strings.Contains(out, label) {
						t.Fatal("tiny width sacrificed usable input to filter label")
					}
				}
			}
		}
	}
}

func TestPrintableSearchFromFullscreenAndPreview(t *testing.T) {
	for _, width := range []int{80, 120} {
		for _, editing := range []tea.KeyMsg{
			{Type: tea.KeyRunes, Runes: []rune("jk")},
			{Type: tea.KeySpace},
			{Type: tea.KeyBackspace},
			{Type: tea.KeyCtrlH},
		} {
			t.Run(fmt.Sprintf("%d/%s", width, editing.String()), func(t *testing.T) {
				p := groupFixture(1, prs.GroupInProgress)
				p.Title = "Change jk"
				m := modelWith(t, Actions{}, p)
				m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
				for range 3 {
					press(m, tea.KeyShiftTab)
				}
				typeQuery(m, "Change")
				press(m, tea.KeyCtrlD)
				m.Update(editing)
				want := "Change"
				switch editing.Type {
				case tea.KeyRunes:
					want += "jk"
				case tea.KeySpace:
					want += " "
				default:
					want = "Chang"
				}
				if m.inDetail || m.previewFocused || m.query != want || m.groupFilter != prs.GroupInProgress || chosen(m) != 1 {
					t.Fatal("fullscreen editing did not preserve group/query and focus list")
				}
				if m.split() {
					press(m, tea.KeyTab)
					m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ZZZ")})
					if m.previewFocused || m.inDetail || m.query != want+"ZZZ" || len(m.rows) != 0 || m.detail != nil || m.detailKey != "" {
						t.Fatal("preview editing retained focus or stale detail on empty result")
					}
				}
			})
		}
	}
}
