package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

func fixture(number int) prs.PullRequest {
	return prs.PullRequest{Repository: "owner/repo", Number: number, Title: fmt.Sprintf("Change %d", number),
		URL: fmt.Sprintf("https://github.com/owner/repo/pull/%d", number), Branch: fmt.Sprintf("topic-%d", number),
		State: "OPEN", MergeState: "BLOCKED", Mergeable: "MERGEABLE", ChecksComplete: true, FeedbackComplete: true}
}

func modelWith(t *testing.T, a Actions, ps ...prs.PullRequest) *Model {
	t.Helper()
	if a.Load == nil {
		a.Load = func(context.Context) (prs.Catalog, error) { return prs.Catalog{PRs: ps}, nil }
	}
	m := NewModel(context.Background(), a)
	t.Cleanup(m.cancel)
	cmd := m.load(false)
	m.Update(cmd())
	return m
}

func press(m *Model, typ tea.KeyType) tea.Cmd {
	_, cmd := m.Update(tea.KeyMsg{Type: typ})
	return cmd
}
func typeQuery(m *Model, s string) { m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}) }
func chosen(m *Model) int          { p, _ := m.selected(); return p.Number }
func plain(m *Model) string        { return ansi.Strip(m.View()) }

// Retained shared fixture; styling/clock tests also use its cached preview.
func renderedPreview(t *testing.T) (*Model, prs.Detail) {
	t.Helper()
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	p := fixture(1)
	m := wideModel(t, Actions{}, p, fixture(2))
	m.markdownProfile = termenv.TrueColor
	d := prs.Detail{PR: p, Body: "# Old styled heading\n\n" + strings.Repeat("paragraph\n\n", 25)}
	finishRender(t, m, m.applyDetail(p.Key(), prs.DetailUpdate{Detail: d, Cached: true}))
	return m, d
}

func TestRankingAndInitialCurrent(t *testing.T) {
	attention, waiting, here := fixture(1), fixture(2), fixture(3)
	attention.Unresolved = 1
	here.Links = []prs.Link{{Current: true, WorkspaceID: "work", TabID: "tab", PaneID: "pane", Agent: true, Label: "checkout"}}
	m := modelWith(t, Actions{}, waiting, here, attention)
	if m.rows[0].Number != 1 || m.rows[1].Number != 3 || chosen(m) != 3 {
		t.Fatalf("attention/current ordering or initial selection: %+v cursor=%d", m.rows, m.cursor)
	}
	typeQuery(m, "WORK pane linked current agent")
	if len(m.rows) != 1 || chosen(m) != 3 {
		t.Fatalf("link search failed: %+v", m.rows)
	}
	press(m, tea.KeyCtrlH)
	if strings.HasSuffix(m.query, "agent") {
		t.Fatal("backspace didn't edit query")
	}
}

func TestFuzzyRanking(t *testing.T) {
	for _, tc := range []struct {
		text, query string
		want        int
		match       bool
	}{
		{"abc", "abc", 0, true}, {"a---b---c", "abc", 24, true},
		{"xxABC", "abc", 2, true}, {"世界 alpha", "世 alpha", 3, true},
		{"alpha beta", "beta alpha", 6, true}, {"abc", "acb", 0, false},
		{"anything", "", 0, true},
	} {
		score, ok := fuzzyScore(tc.text, tc.query)
		if ok != tc.match || ok && score != tc.want {
			t.Errorf("%q / %q: %d,%v", tc.text, tc.query, score, ok)
		}
	}
	a, b := fixture(1), fixture(2)
	a.Title, a.Unresolved = "a---b---c", 1
	b.Title = "abc"
	m := modelWith(t, Actions{}, a, b)
	typeQuery(m, "abc")
	if chosen(m) != 2 {
		t.Fatal("tight fuzzy match must beat attention priority")
	}
}

func TestRefreshFailureRetainsSelectionAndIgnoresOldLoad(t *testing.T) {
	m := modelWith(t, Actions{}, fixture(1), fixture(2))
	press(m, tea.KeyDown)
	m.actions.Load = func(context.Context) (prs.Catalog, error) { return prs.Catalog{}, errors.New("offline") }
	cmd := press(m, tea.KeyCtrlR)
	if !m.loading || chosen(m) != 2 {
		t.Fatal("refresh changed selection")
	}
	msg := cmd()
	if m.stale {
		t.Fatal("command mutated model before delivery")
	}
	_, follow := m.Update(msg)
	if follow != nil || m.quitting || !m.stale || chosen(m) != 2 || len(m.rows) != 2 {
		t.Fatal("refresh failure lost popup state")
	}
	if strings.Contains(plain(m), "STALE") || strings.Contains(plain(m), "Loading") || !strings.Contains(plain(m), "offline") {
		t.Fatal(plain(m))
	}
	// Complete the old failed request but delay its delivery until success.
	oldFailure := press(m, tea.KeyCtrlR)()
	m.actions.Load = func(context.Context) (prs.Catalog, error) {
		return prs.Catalog{PRs: []prs.PullRequest{fixture(3), fixture(2)}}, nil
	}
	fresh := press(m, tea.KeyCtrlR)
	freshMsg := fresh()
	m.Update(freshMsg)
	m.Update(oldFailure)
	if m.stale || m.loading || chosen(m) != 2 || m.err != "" {
		t.Fatal("fresh catalog didn't clear stale state/preserve key")
	}
}

func TestDetailRaceCancelBackAndNextRow(t *testing.T) {
	m := modelWith(t, Actions{Detail: func(ctx context.Context, p prs.PullRequest) (prs.Detail, error) {
		return prs.Detail{PR: p, Body: p.Title}, nil
	}}, fixture(1), fixture(2))
	old := press(m, tea.KeyCtrlD)
	oldToken := m.detailToken
	press(m, tea.KeyEsc)
	press(m, tea.KeyDown)
	fresh := press(m, tea.KeyEnter)
	m.Update(fresh())
	m.Update(old())
	m.Update(detailMsg{token: oldToken, key: fixture(1).Key(), err: errors.New("late failure")})
	if m.detail == nil || m.detail.PR.Number != 2 || m.detailErr != "" || m.err != "" {
		t.Fatal("late details replaced next row or surfaced a stale error")
	}
	press(m, tea.KeyEsc)
	if m.inDetail || chosen(m) != 2 || m.quitting {
		t.Fatal("Esc detail didn't restore list selection")
	}
}

func TestBlockingDetailIsCancelledWithoutWaiting(t *testing.T) {
	started := make(chan struct{})
	results := make(chan tea.Msg, 1)
	m := modelWith(t, Actions{Detail: func(ctx context.Context, p prs.PullRequest) (prs.Detail, error) {
		close(started)
		<-ctx.Done()
		return prs.Detail{}, ctx.Err()
	}}, fixture(1))
	cmd := press(m, tea.KeyCtrlD)
	go func() { results <- cmd() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("command didn't start")
	}
	press(m, tea.KeyEsc)
	select {
	case msg := <-results:
		m.Update(msg)
	case <-time.After(time.Second):
		t.Fatal("detail context not cancelled on back")
	}
	if m.inDetail || m.err != "" {
		t.Fatal("cancelled detail surfaced an error")
	}
}

func TestDetailFailureRetryAndReadyUpdatesCatalog(t *testing.T) {
	p := fixture(1)
	p.Links = []prs.Link{{WorkspaceID: "local", Current: true}}
	m := modelWith(t, Actions{Detail: func(context.Context, prs.PullRequest) (prs.Detail, error) { return prs.Detail{}, errors.New("denied") }}, p, fixture(2))
	m.Update(press(m, tea.KeyCtrlD)())
	if !m.inDetail || m.detailLoading || m.quitting || !strings.Contains(plain(m), "denied") {
		t.Fatal(plain(m))
	}
	if !strings.Contains(plain(m), "Details unavailable") {
		t.Fatal("missing retry prompt")
	}
	m.actions.Detail = func(context.Context, prs.PullRequest) (prs.Detail, error) {
		fresh := p
		fresh.MergeState, fresh.Links = "CLEAN", nil
		return prs.Detail{PR: fresh, Body: "fresh body"}, nil
	}
	_, fetch := m.Update(press(m, tea.KeyCtrlR)())
	_, render := m.Update(fetch())
	finishRender(t, m, render)
	if m.detail.PR.Status() != "ready" || len(m.detail.PR.Links) != 1 || m.err != "" {
		t.Fatal("fresh detail didn't update readiness/link metadata")
	}
	press(m, tea.KeyEsc)
	if chosen(m) != 1 || m.rows[0].Status() != "ready" || !strings.Contains(plain(m), "ready") {
		t.Fatal("catalog still contains stale status")
	}
}

func TestRefreshInvalidatesDetailAndRefetches(t *testing.T) {
	p := fixture(1)
	m := modelWith(t, Actions{Detail: func(context.Context, prs.PullRequest) (prs.Detail, error) { return prs.Detail{PR: p, Body: "old"}, nil }}, p)
	old := press(m, tea.KeyCtrlD)
	m.actions.Detail = func(context.Context, prs.PullRequest) (prs.Detail, error) {
		fresh := p
		fresh.MergeState = "CLEAN"
		return prs.Detail{PR: fresh, Body: "new"}, nil
	}
	load := press(m, tea.KeyCtrlR)
	m.Update(old())
	if m.detail != nil {
		t.Fatal("old detail accepted during refresh")
	}
	_, detail := m.Update(load())
	if detail == nil {
		t.Fatal("refresh didn't reload active detail")
	}
	m.Update(detail())
	if m.detail.PR.Status() != "ready" || m.detail.Body != "new" {
		t.Fatal("refreshed detail isn't fresh")
	}
}

func TestEnterJumpAndExplicitBrowser(t *testing.T) {
	p := fixture(1)
	p.Links = []prs.Link{{WorkspaceID: "other"}, {WorkspaceID: "current", Current: true}}
	var jumps, opens int
	fail := true
	m := modelWith(t, Actions{
		Jump: func(_ context.Context, link prs.Link) error {
			jumps++
			if link.WorkspaceID != "current" {
				t.Fatal("wrong preferred checkout")
			}
			if fail {
				return errors.New("jump failed")
			}
			return nil
		},
		OpenURL: func(_ context.Context, url string) error {
			opens++
			if url != p.URL {
				t.Fatal(url)
			}
			return nil
		},
	}, p)
	_, quit := m.Update(press(m, tea.KeyEnter)())
	if quit != nil || m.quitting || jumps != 1 || opens != 0 || chosen(m) != 1 {
		t.Fatal("failed jump changed focus/closed/opened browser")
	}
	m.Update(press(m, tea.KeyCtrlO)())
	if m.quitting || opens != 1 || m.inDetail {
		t.Fatal("explicit browser must leave popup open")
	}
	fail = false
	_, quit = m.Update(press(m, tea.KeyEnter)())
	if !m.quitting || quit == nil {
		t.Fatal("successful jump didn't exit")
	}
	if _, ok := quit().(tea.QuitMsg); !ok {
		t.Fatal("not a quit command")
	}
}

func TestUnlinkedEnterNeverOpensBrowserAndActionErrorsPersist(t *testing.T) {
	var opens int
	m := modelWith(t, Actions{
		Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
			return prs.Detail{PR: p, Body: "body"}, nil
		},
		OpenURL: func(context.Context, string) error { opens++; return errors.New("browser failed") },
	}, fixture(1), fixture(2))
	m.Update(press(m, tea.KeyEnter)())
	if !m.inDetail || opens != 0 {
		t.Fatal("unlinked Enter must only show detail")
	}
	if cmd := press(m, tea.KeyEnter); cmd != nil {
		t.Fatal("Enter in details should not jump")
	}
	m.Update(press(m, tea.KeyCtrlO)())
	if !m.inDetail || m.quitting || !strings.Contains(plain(m), "browser failed") {
		t.Fatal("browser failure dismissed detail")
	}
	press(m, tea.KeyEsc)
	press(m, tea.KeyDown)
	if chosen(m) != 2 || !strings.Contains(plain(m), "browser failed") {
		t.Fatal("navigation hid failure or changed target")
	}
}

func TestStaleActionCannotCloseNewSelection(t *testing.T) {
	p := fixture(1)
	p.Links = []prs.Link{{WorkspaceID: "one"}}
	called := false
	m := modelWith(t, Actions{Jump: func(context.Context, prs.Link) error {
		called = true
		return nil
	}}, p, fixture(2))
	old := press(m, tea.KeyEnter)
	press(m, tea.KeyDown)
	_, cmd := m.Update(old())
	if m.quitting || cmd != nil || chosen(m) != 2 || called {
		t.Fatal("stale action executed or closed new selection")
	}
}

func TestListNavigationResizeAndFooter(t *testing.T) {
	var ps []prs.PullRequest
	for i := 1; i <= 12; i++ {
		ps = append(ps, fixture(i))
	}
	m := modelWith(t, Actions{}, ps...)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 9})
	for _, text := range []string{"Enter jump/details", "Tab preview", "^D full", "^O web", "^R refresh", "repo #1", "topic-1", "12/12 items", "│"} {
		if !strings.Contains(plain(m), text) {
			t.Fatalf("missing %q in %s", text, plain(m))
		}
	}
	if strings.Contains(plain(m), "in review") || strings.Contains(plain(m), "unlinked") {
		t.Fatal("list exposes redundant review/linkage badges: " + plain(m))
	}
	press(m, tea.KeyPgDown)
	if m.contentHeight() != 4 || m.cursor != 3 {
		t.Fatal("page navigation must budget a heading and three one-line items")
	}
	assertListSelection(t, m)
	press(m, tea.KeyEnd)
	if m.cursor != 11 || !strings.Contains(plain(m), m.rows[m.cursor].Title) {
		t.Fatal("last cursor not visible")
	}
	assertListSelection(t, m)
	press(m, tea.KeyPgUp)
	if m.cursor != 8 {
		t.Fatal("page up")
	}
	press(m, tea.KeyHome)
	if m.cursor != 0 || m.top != 0 {
		t.Fatal("home")
	}
	for _, size := range []tea.WindowSizeMsg{{Width: 1, Height: 1}, {Width: 2, Height: 2}, {Width: 9, Height: 4}, {Width: 20, Height: 7}, {Width: 0, Height: 0}} {
		m.Update(size)
		press(m, tea.KeyEnd)
		assertDimensions(t, m)
		assertListSelection(t, m)
	}
}

func assertDimensions(t *testing.T, m *Model) {
	t.Helper()
	lines := strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Fatalf("height %d != %d", len(lines), m.height)
	}
	for _, line := range lines {
		if width := ansi.StringWidth(line); width > m.width {
			t.Fatalf("width %d > %d: %q", width, m.width, line)
		}
	}
}

func TestDetailContentScrollAndUnicodeSafety(t *testing.T) {
	p := fixture(1)
	p.Title = "界👩‍💻e\u0301 title\x1b]52;c;payload\a\x1b[31mred\x1b[0m\u202e\x00\u0085"
	p.Checks = []prs.Check{{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"}}
	d := prs.Detail{PR: p, Body: strings.Repeat("界👩‍💻e\u0301 paragraph\n", 8),
		Reviews:  []prs.Review{{Author: "reviewer", State: "APPROVED", Body: "review text"}},
		Threads:  []prs.Thread{{Path: "open.go", Line: 9, Comments: []prs.Comment{{Author: "author", Body: "unresolved comment"}}}, {Path: "hidden.go", Resolved: true}},
		Comments: []prs.Comment{{Author: "person", CreatedAt: "today", Body: "conversation text"}}, Warnings: []string{"partial response"}}
	m := modelWith(t, Actions{Detail: func(context.Context, prs.PullRequest) (prs.Detail, error) { return d, nil }}, p)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 12})
	_, render := m.Update(press(m, tea.KeyCtrlD)())
	finishRender(t, m, render)
	joined := strings.Join(m.detailLines(), "\n")
	for _, text := range []string{"Body", "Checks", "SUCCESS", "Reviews", "APPROVED", "Unresolved threads", "open.go:9", "Conversation", "conversation text", "Warnings", "partial response"} {
		if !strings.Contains(joined, text) {
			t.Fatalf("missing detail %q", text)
		}
	}
	if strings.Contains(joined, "hidden.go") || strings.Contains(joined, "payload") {
		t.Fatal("resolved thread/terminal payload leaked")
	}
	if !strings.Contains(plain(m), "Esc/Back back") || !strings.Contains(plain(m), "^D back") || strings.Contains(plain(m), "retry") {
		t.Fatal("detail footer")
	}
	press(m, tea.KeyPgDown)
	if m.detailTop != m.contentHeight() {
		t.Fatal("detail page down")
	}
	press(m, tea.KeyEnd)
	if m.detailTop != len(m.detailLines())-m.contentHeight() {
		t.Fatal("detail end")
	}
	press(m, tea.KeyHome)
	if m.detailTop != 0 {
		t.Fatal("detail home")
	}
	for width := 1; width <= 20; width++ {
		_, render := m.Update(tea.WindowSizeMsg{Width: width, Height: 8})
		finishRender(t, m, render)
		press(m, tea.KeyEnd)
		assertDimensions(t, m)
	}
}

func TestSanitizePreservesUnicodeNotTerminalControls(t *testing.T) {
	input := "safe\x1b[31mred\x1b[0m\x1b]0;evil title\a\x1b]8;;https://evil\x1b\\label\x1b]8;;\x1b\\\u202e\u2066\u061c\x00\x7f\u0085\n界👩‍💻e\u0301\tend"
	got := sanitize(input)
	if got != "saferedlabel\n界👩‍💻e\u0301    end" {
		t.Fatalf("sanitize: %q", got)
	}
	for _, r := range singleLine(input) {
		if unicode.IsControl(r) {
			t.Fatalf("control survived: %U", r)
		}
	}
	for width := 1; width < 15; width++ {
		if ansi.StringWidth(fit(input, width)) > width {
			t.Fatalf("unicode truncation overflow at %d", width)
		}
	}
	if got := fit("e\u0301👩‍💻界", 3); got != "e\u0301…" {
		t.Fatalf("grapheme truncation: %q", got)
	}
}

func TestMissingActionsEmptySearchAndQuitCancellation(t *testing.T) {
	m := NewModel(context.Background(), Actions{})
	t.Cleanup(m.cancel)
	m.Update(m.load(false)())
	if m.quitting || m.loading || !strings.Contains(plain(m), "unavailable") {
		t.Fatal("nil load should show an error")
	}
	for _, key := range []tea.KeyType{tea.KeyEnter, tea.KeyCtrlD, tea.KeyCtrlO, tea.KeyDown, tea.KeyEnd} {
		if press(m, key) != nil {
			t.Fatal("empty catalog scheduled an action")
		}
	}
	typeQuery(m, "qjk界")
	if m.query != "qjk界" || m.quitting {
		t.Fatal("printable keys are search, not shortcuts")
	}
	press(m, tea.KeyBackspace)
	if m.query != "qjk" {
		t.Fatal("unicode backspace")
	}
	quit := press(m, tea.KeyEsc)
	if quit == nil || !m.quitting || m.ctx.Err() != context.Canceled {
		t.Fatal("Esc didn't exit/cancel requests")
	}
}

func TestParentCancellationPropagatesToRunningCommands(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, results := make(chan context.Context, 1), make(chan tea.Msg, 1)
	m := NewModel(ctx, Actions{Load: func(ctx context.Context) (prs.Catalog, error) {
		started <- ctx
		<-ctx.Done()
		return prs.Catalog{}, ctx.Err()
	}})
	t.Cleanup(m.cancel)
	cmd := m.load(false)
	go func() { results <- cmd() }()
	var request context.Context
	select {
	case request = <-started:
	case <-time.After(time.Second):
		t.Fatal("load callback never ran")
	}
	cancel()
	select {
	case msg := <-results:
		m.Update(msg)
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not unblock load")
	}
	if request.Err() != context.Canceled || m.loading || m.ctx.Err() != context.Canceled {
		t.Fatal("parent cancellation failed to reach running command/model")
	}
}

func TestNarrowPendingAndFailedDetail(t *testing.T) {
	m := modelWith(t, Actions{}, fixture(1))
	for width := 1; width < 20; width++ {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 7})
		cmd := press(m, tea.KeyCtrlD)
		assertDimensions(t, m)
		m.Update(cmd())
		assertDimensions(t, m)
		if press(m, tea.KeyCtrlD) != nil || m.inDetail {
			t.Fatal("fullscreen close retried failed detail")
		}
	}
}

func TestRemovedDetailCannotActAfterRefresh(t *testing.T) {
	p := fixture(1)
	m := modelWith(t, Actions{Detail: func(context.Context, prs.PullRequest) (prs.Detail, error) {
		return prs.Detail{PR: p, Body: "old"}, nil
	}}, p)
	m.Update(press(m, tea.KeyCtrlD)())
	m.actions.Load = func(context.Context) (prs.Catalog, error) {
		return prs.Catalog{PRs: []prs.PullRequest{fixture(2)}}, nil
	}
	_, cmd := m.Update(press(m, tea.KeyCtrlR)())
	if cmd != nil || m.detail != nil || !m.inDetail || !strings.Contains(plain(m), "no longer in catalog") {
		t.Fatal("removed PR survived refresh")
	}
	if press(m, tea.KeyCtrlO) != nil {
		t.Fatal("removed PR remained actionable")
	}
	if press(m, tea.KeyCtrlD) != nil || m.inDetail || chosen(m) != 2 || m.quitting || m.detail != nil {
		t.Fatal("fullscreen toggle could not return to fresh list without fetching removed PR")
	}
}

// Real Bubble Tea scheduling, without a terminal/renderer. Quit must return nil,
// not ErrProgramKilled, even though it cancels outstanding request contexts.
func TestHeadlessProgramQuitCancelsRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started, finished := make(chan struct{}), make(chan struct{})
	m := NewModel(ctx, Actions{Load: func(ctx context.Context) (prs.Catalog, error) {
		close(started)
		defer close(finished)
		<-ctx.Done()
		return prs.Catalog{}, ctx.Err()
	}})
	defer m.cancel()
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler(), tea.WithAltScreen())
	result := make(chan error, 1)
	go func() { _, err := p.Run(); result <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("program load didn't start")
	}
	p.Send(tea.KeyMsg{Type: tea.KeyEsc})
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("intentional quit returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal("program quit waited for request")
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("program quit didn't cancel request")
	}
}
