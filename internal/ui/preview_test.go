package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/owenps/herdr-prs/internal/prs"
)

// Most tests deliver timer messages directly; one exercises the real delay.
func previewRequest(t *testing.T, m *Model) tea.Cmd {
	t.Helper()
	_, cmd := m.Update(previewMsg{token: m.detailToken, key: m.detailKey})
	if cmd == nil {
		t.Fatal("preview did not schedule a request")
	}
	return cmd
}

func wideModel(t *testing.T, a Actions, ps ...prs.PullRequest) *Model {
	t.Helper()
	m := modelWith(t, a, ps...)
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 12})
	if len(ps) > 0 && cmd == nil {
		t.Fatal("wide resize did not schedule preview")
	}
	return m
}

func TestLoadingNoticeFollowsCatalogDebounceAndDetailRequests(t *testing.T) {
	for _, width := range []int{80, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			loadGate, detailGate := make(chan struct{}), make(chan struct{})
			started := make(chan struct{}, 1)
			var calls atomic.Int32
			h := newStreamHarness(t, Actions{
				Load: func(ctx context.Context) (prs.Catalog, error) {
					select {
					case <-ctx.Done():
						return prs.Catalog{}, ctx.Err()
					case <-loadGate:
						return prs.Catalog{PRs: []prs.PullRequest{fixture(1)}}, nil
					}
				},
				Detail: func(ctx context.Context, _ prs.PullRequest) (prs.Detail, error) {
					calls.Add(1)
					started <- struct{}{}
					select {
					case <-ctx.Done():
						return prs.Detail{}, ctx.Err()
					case <-detailGate:
						return prs.Detail{}, errors.New("details offline")
					}
				},
			})
			h.deliver(tea.WindowSizeMsg{Width: width, Height: 12})
			h.start(h.m.Init())
			if strings.Count(plain(h.m), "Loading…") != 1 {
				t.Fatal("catalog request missing its single global loading notice")
			}
			close(loadGate)
			h.until(t, func() bool { return !h.m.loading })
			if strings.Contains(plain(h.m), "Loading") || calls.Load() != 0 {
				t.Fatal("narrow list/debounced preview pretended to be an active request")
			}
			if width == 80 {
				h.deliver(tea.KeyMsg{Type: tea.KeyCtrlD})
			}
			h.until(t, func() bool { return h.m.detailLoading && !h.m.detailDebouncing })
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("detail command did not start")
			}
			if strings.Count(plain(h.m), "Loading…") != 1 {
				t.Fatal("detail request missing its single global notice")
			}
			close(detailGate)
			h.until(t, func() bool { return !h.m.detailLoading })
			view := plain(h.m)
			if strings.Contains(view, "Loading") || strings.Contains(view, "STALE") || !strings.Contains(view, "details offline") || !strings.Contains(view, "Details unavailable") || !strings.Contains(view, "^R refresh") || width == 80 && !strings.Contains(view, "Ctrl-R to retry") {
				t.Fatal(view)
			}
			assertDimensions(t, h.m)
		})
	}
}

func TestPreviewInitialLoadAndDebounce(t *testing.T) {
	var calls int
	p := fixture(1)
	m := NewModel(context.Background(), Actions{
		Load: func(context.Context) (prs.Catalog, error) { return prs.Catalog{PRs: []prs.PullRequest{p}}, nil },
		Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
			calls++
			return prs.Detail{PR: p, Body: "initial preview"}, nil
		},
	})
	t.Cleanup(m.cancel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 12})
	_, timer := m.Update(m.load(false)())
	if timer == nil || !m.detailLoading || m.inDetail || calls != 0 {
		t.Fatal("initial wide load did not debounce automatically")
	}
	start := time.Now()
	msg := timer()
	if time.Since(start) < previewDelay/2 || calls != 0 {
		t.Fatal("timer called API or did not debounce")
	}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("current timer discarded")
	}
	result := cmd()
	if m.detail != nil {
		t.Fatal("command mutated model")
	}
	_, render := m.Update(result)
	finishRender(t, m, render)
	if calls != 1 || m.detailLoading || m.inDetail || !strings.Contains(plain(m), "initial preview") {
		t.Fatal(plain(m))
	}
	assertDimensions(t, m)
}

func TestPreviewDiscardedDebounceAndNoOldContent(t *testing.T) {
	var calls int
	m := wideModel(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		calls++
		return prs.Detail{PR: p, Body: "old-content-1"}, nil
	}}, fixture(1), fixture(2), fixture(3))
	firstTimer := previewMsg{token: m.detailToken, key: m.detailKey}
	press(m, tea.KeyDown)
	secondTimer := previewMsg{token: m.detailToken, key: m.detailKey}
	press(m, tea.KeyDown)
	for _, msg := range []previewMsg{firstTimer, secondTimer} {
		if _, cmd := m.Update(msg); cmd != nil {
			t.Fatal("discarded debounce started API")
		}
	}
	if calls != 0 || m.detailKey != fixture(3).Key() {
		t.Fatal("debounce lost selected key")
	}
	m.Update(previewRequest(t, m)())
	if calls != 1 {
		t.Fatal("more than final selection fetched")
	}
	press(m, tea.KeyHome)
	if m.detail != nil || strings.Contains(plain(m), "old-content-1") || strings.Contains(plain(m), "Loading") {
		t.Fatal("next PR displayed old content")
	}
}

func TestPreviewRapidSelectionCancelsAndRejectsLateResults(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	results := make(chan tea.Msg, 1)
	m := wideModel(t, Actions{Detail: func(ctx context.Context, p prs.PullRequest) (prs.Detail, error) {
		if p.Number == 1 {
			close(started)
			<-ctx.Done()
			close(cancelled)
			// Simulate an API that returns success despite cancellation.
			return prs.Detail{PR: p, Body: "late-old-content"}, nil
		}
		return prs.Detail{PR: p, Body: "selected-content"}, nil
	}}, fixture(1), fixture(2))
	cmd := previewRequest(t, m)
	oldToken := m.detailToken
	go func() { results <- cmd() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	press(m, tea.KeyDown)
	if m.detail != nil {
		t.Fatal("old body retained while new selection pending")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("selection did not cancel old request")
	}
	m.Update(previewRequest(t, m)())
	m.Update(<-results)
	m.Update(detailMsg{token: oldToken, key: fixture(1).Key(), err: errors.New("late error")})
	if m.detail.PR.Number != 2 || m.detailErr != "" || strings.Contains(plain(m), "late-old-content") || len(m.detailCache) != 1 {
		t.Fatal("stale result affected preview or cache")
	}
	// A -> B -> A: identity alone must not validate an old A response.
	press(m, tea.KeyUp)
	m.Update(detailMsg{token: oldToken, key: fixture(1).Key(), detail: prs.Detail{PR: fixture(1), Body: "late-old-content"}})
	if m.detail != nil || !m.detailLoading {
		t.Fatal("same-key stale generation accepted")
	}
}

func TestPreviewCacheAndStableRows(t *testing.T) {
	calls := map[int]int{}
	m := wideModel(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		calls[p.Number]++
		p.Unresolved = 1 // Would move PR 2 ahead of PR 1 if reranked.
		p.Links = nil
		return prs.Detail{PR: p, Body: fmt.Sprintf("body-%d", p.Number)}, nil
	}}, fixture(1), fixture(2))
	_, render := m.Update(previewRequest(t, m)())
	finishRender(t, m, render)
	press(m, tea.KeyDown)
	cursor, top := m.cursor, m.top
	_, render = m.Update(previewRequest(t, m)())
	finishRender(t, m, render)
	if chosen(m) != 2 || m.rows[0].Number != 1 || m.cursor != cursor || m.top != top || m.rows[1].Status() != "feedback" {
		t.Fatal("detail result reordered rows or lost new metadata")
	}
	if cmd := press(m, tea.KeyUp); cmd != nil {
		t.Fatal("cached selection scheduled API")
	}
	if m.detail.PR.Number != 1 || calls[1] != 1 || m.detailLoading || !strings.Contains(plain(m), "body-1") {
		t.Fatal("selection did not reuse cache")
	}
	if cmd := press(m, tea.KeyDown); cmd != nil || m.detail.PR.Number != 2 || calls[2] != 1 || !strings.Contains(plain(m), "body-2") {
		t.Fatal("second selection did not reuse cache")
	}
	// Returning from fullscreen to wide list also reuses fresh detail.
	finishRender(t, m, press(m, tea.KeyCtrlD))
	cmd := press(m, tea.KeyEsc)
	if m.inDetail || m.detail.PR.Number != 2 {
		t.Fatal("fullscreen return discarded usable preview cache")
	}
	finishRender(t, m, cmd) // Rendering is local; no second API request.
}

func TestPreviewSearchAndEmptySelection(t *testing.T) {
	m := wideModel(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		return prs.Detail{PR: p, Body: "previous body"}, nil
	}}, fixture(1), fixture(2))
	m.Update(previewRequest(t, m)())
	_, timer := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Change 2")})
	if timer == nil || chosen(m) != 2 || m.detail != nil || strings.Contains(plain(m), "previous body") {
		t.Fatal("search failed to sync preview")
	}
	pending := previewMsg{token: m.detailToken, key: m.detailKey}
	typeQuery(m, "nonexistent")
	if m.detailKey != "" || m.detailLoading || !strings.Contains(plain(m), "Select an item") {
		t.Fatal("empty search retained preview")
	}
	if _, cmd := m.Update(pending); cmd != nil {
		t.Fatal("empty search accepted pending debounce")
	}
	for range "nonexistent" {
		press(m, tea.KeyBackspace)
	}
	if !m.detailLoading || m.detailKey != fixture(2).Key() {
		t.Fatal("backspace did not restore auto preview")
	}
}

func TestPreviewRefreshFreshnessAndErrors(t *testing.T) {
	generation := "old"
	fail := false
	m := wideModel(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		if fail {
			return prs.Detail{}, errors.New("denied\x1b]52;c;evil\a\u202e")
		}
		return prs.Detail{PR: p, Body: generation + " body"}, nil
	}}, fixture(1), fixture(2))
	_, render := m.Update(previewRequest(t, m)())
	finishRender(t, m, render)
	press(m, tea.KeyDown)
	_, render = m.Update(previewRequest(t, m)())
	finishRender(t, m, render)
	old := detailMsg{token: m.detailToken, key: m.detailKey, detail: *m.detail}
	generation = "fresh"
	load := press(m, tea.KeyCtrlR)
	if len(m.detailCache) != 0 || !m.detailStale || strings.Contains(plain(m), "STALE") || strings.Count(plain(m), "Loading…") != 1 {
		t.Fatal("refresh did not invalidate cache/mark retained body")
	}
	m.Update(old)
	_, timer := m.Update(load())
	if timer == nil || chosen(m) != 2 {
		t.Fatal("refresh lost selection or did not reload preview")
	}
	fail = true
	m.Update(previewRequest(t, m)())
	view := plain(m)
	if !m.detailStale || m.detailLoading || strings.Contains(view, "Loading") || strings.Contains(view, "STALE") || !strings.Contains(view, "denied") || !strings.Contains(view, "old body") || strings.Contains(view, "evil") || strings.Contains(view, "\u202e") {
		t.Fatal(view)
	}
	fail = false
	_, timer = m.Update(press(m, tea.KeyCtrlR)())
	if timer == nil {
		t.Fatal("refresh retry not scheduled")
	}
	_, render = m.Update(previewRequest(t, m)())
	finishRender(t, m, render)
	if m.detailStale || m.detailErr != "" || !strings.Contains(plain(m), "fresh body") {
		t.Fatal("successful retry remained stale")
	}
	press(m, tea.KeyUp)
	if m.detail != nil || !m.detailLoading {
		t.Fatal("refresh retained other PR cache")
	}
	fail = true
	m.Update(previewRequest(t, m)())
	if m.detail != nil || !strings.Contains(plain(m), "Details unavailable") || !strings.Contains(plain(m), "denied") {
		t.Fatal("initial lookup failure hidden")
	}
}

func TestPreviewResizeAndFullscreenFallback(t *testing.T) {
	m := modelWith(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		return prs.Detail{PR: p, Body: "safe body"}, nil
	}}, fixture(1))
	if m.detailLoading || m.detail != nil {
		t.Fatal("narrow list auto-loaded detail")
	}
	for _, width := range []int{99, 100, 101, 140, 250, 100} {
		_, cmd := m.Update(tea.WindowSizeMsg{Width: width, Height: 8})
		if m.split() != (width >= 100) {
			t.Fatal("wrong split threshold")
		}
		if width == 100 && m.detail == nil && cmd != nil {
			_, cmd = m.Update(previewRequest(t, m)())
		}
		finishRender(t, m, cmd)
		assertDimensions(t, m)
	}
	press(m, tea.KeyTab)
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 99, Height: 8})
	if m.previewFocused || m.split() || cmd != nil || m.detail != nil {
		t.Fatal("shrinking did not return to list")
	}
	finishRender(t, m, press(m, tea.KeyCtrlD))
	if !m.inDetail || m.detail == nil || m.detail.Body != "safe body" {
		t.Fatal("narrow full detail unavailable")
	}
	press(m, tea.KeyPgDown)
	press(m, tea.KeyDown)
	if m.detailTop == 0 || !strings.Contains(plain(m), "safe body") {
		t.Fatal("narrow detail could not scroll past metadata with the global chrome budget")
	}
	press(m, tea.KeyEsc)
	_, cmd = m.Update(tea.WindowSizeMsg{Width: 100, Height: 8})
	if m.detail == nil {
		t.Fatal("widening did not reuse cache")
	}
	finishRender(t, m, cmd) // New pane width can require local Markdown rendering.
	// Shrinking before a timer/request returns rejects its generation.
	load := press(m, tea.KeyCtrlR)
	m.Update(load())
	pending := previewMsg{token: m.detailToken, key: m.detailKey}
	request := previewRequest(t, m)
	m.Update(tea.WindowSizeMsg{Width: 99, Height: 8})
	if _, cmd := m.Update(pending); cmd != nil {
		t.Fatal("hidden preview timer called API")
	}
	m.Update(request())
	if m.detail != nil || m.detailErr != "" {
		t.Fatal("hidden preview accepted response")
	}
}

func TestPreviewFocusScrollNavigationAndJump(t *testing.T) {
	p := fixture(1)
	p.Links = []prs.Link{{WorkspaceID: "local", Current: true}}
	var jumps int
	m := wideModel(t, Actions{
		Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
			return prs.Detail{PR: p, Body: strings.Repeat("long paragraph\n", 40)}, nil
		},
		Jump: func(context.Context, prs.Link) error { jumps++; return nil },
	}, p, fixture(2))
	_, render := m.Update(previewRequest(t, m)())
	finishRender(t, m, render)
	press(m, tea.KeyTab)
	if !m.previewFocused || !strings.Contains(plain(m), "Details ·") || strings.Contains(plain(m), "●") || strings.Contains(plain(m), "focused") {
		t.Fatal("Tab did not focus preview")
	}
	press(m, tea.KeyDown)
	if m.detailTop != 1 || chosen(m) != 1 {
		t.Fatal("focused arrow moved list")
	}
	press(m, tea.KeyPgDown)
	if m.detailTop != 1+m.contentHeight() {
		t.Fatal("focused page did not scroll")
	}
	press(m, tea.KeyEnd)
	if m.detailTop != len(m.detailLines())-m.contentHeight() {
		t.Fatal("focused end did not scroll")
	}
	if press(m, tea.KeyEnter) != nil || jumps != 0 {
		t.Fatal("focused preview jumped")
	}
	press(m, tea.KeyTab)
	press(m, tea.KeyDown)
	if chosen(m) != 2 || m.previewFocused || m.detailTop != 0 {
		t.Fatal("Tab did not restore list navigation")
	}
	press(m, tea.KeyTab)
	press(m, tea.KeyEsc)
	if m.previewFocused || m.quitting {
		t.Fatal("first Esc should return list focus")
	}
	press(m, tea.KeyUp)
	m.Update(press(m, tea.KeyEnter)())
	if jumps != 1 || !m.quitting {
		t.Fatal("list Enter no longer jumps linked PR")
	}
	other := wideModel(t, Actions{}, fixture(1))
	press(other, tea.KeyTab)
	press(other, tea.KeyEsc)
	press(other, tea.KeyEsc)
	if !other.quitting || other.ctx.Err() != context.Canceled {
		t.Fatal("second Esc did not exit/cancel")
	}
}

func TestPreviewUnicodeDimensionsAndActionErrorIsolation(t *testing.T) {
	p := fixture(1)
	p.Title = "界👩‍💻e\u0301 title\x1b]52;c;evil\a\u202e"
	p.Links = []prs.Link{{Label: "local\x1b[31mred\x1b[0m"}}
	m := wideModel(t, Actions{
		Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
			return prs.Detail{PR: p, Body: strings.Repeat("界👩‍💻e\u0301\x1b]0;evil\a", 60)}, nil
		},
		OpenURL: func(context.Context, string) error { return errors.New("browser failed") },
	}, p)
	m.Update(press(m, tea.KeyCtrlO)())
	m.Update(previewRequest(t, m)())
	if !strings.Contains(plain(m), "browser failed") || strings.Contains(plain(m), "evil") || len(m.detail.PR.Links) != 1 {
		t.Fatal("preview cleared action error/local links or leaked controls")
	}
	for _, width := range []int{100, 101, 120, 200} {
		for _, height := range []int{1, 2, 3, 4, 12} {
			m.Update(tea.WindowSizeMsg{Width: width, Height: height})
			assertDimensions(t, m)
			_, right := m.paneWidths()
			for _, line := range m.detailLines() {
				if ansi.StringWidth(line) > right {
					t.Fatal("detail wrapped at terminal rather than pane width")
				}
			}
		}
	}
}
