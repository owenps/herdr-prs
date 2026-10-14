package ui

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

func agentFixture(number int, status prs.AgentStatus) prs.PullRequest {
	p := fixture(number)
	p.Links = []prs.Link{{WorkspaceID: "workspace", TabID: "tab", PaneID: "pane", Path: "/work/checkout", Agent: status != prs.AgentNone, AgentStatus: status}}
	return p
}

func agentSnapshot(status prs.AgentStatus) prs.Snapshot {
	return prs.Snapshot{Panes: []prs.Pane{{ID: "pane", WorkspaceID: "workspace", TabID: "tab", CWD: "/work/checkout", Agent: "claude", AgentStatus: status}}}
}

func acceptAgents(t *testing.T, m *Model, snapshot prs.Snapshot, err error) tea.Cmd {
	t.Helper()
	if !m.agentPolling {
		if m.pollAgents() == nil {
			t.Fatal("agent polling not enabled")
		}
	}
	_, cmd := m.Update(agentSnapshotMsg{m.agentToken, snapshot, err})
	return cmd
}

func fakeAgentSnapshot(context.Context) (prs.Snapshot, error) { return prs.Snapshot{}, nil }

func agentPreviewPair() (prs.PullRequest, prs.PullRequest, prs.Snapshot) {
	p, next := agentFixture(1, prs.AgentWorking), agentFixture(2, prs.AgentWorking)
	// Different checkouts/identities; no incidental fuzzy matches for "agent".
	for _, row := range []*prs.PullRequest{&p, &next} {
		row.Title, row.Branch, row.MergeState = "Fix", "fix", "CLEAN"
	}
	next.Links[0].PaneID, next.Links[0].Path = "pane-2", "/work/checkout-2"
	snapshot := agentSnapshot(prs.AgentWorking)
	pane := snapshot.Panes[0]
	pane.ID, pane.CWD = next.Links[0].PaneID, next.Links[0].Path
	snapshot.Panes = append(snapshot.Panes, pane)
	return p, next, snapshot
}

func TestNativeAgentDotsAndIsolatedColors(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	for _, tc := range []struct {
		status      prs.AgentStatus
		icon, color string
	}{
		{prs.AgentBlocked, "●", "#bf616a"},
		{prs.AgentWorking, "●", "#ebcb8b"},
		{prs.AgentDone, "●", "#8fbcbb"},
		{prs.AgentIdle, "○", "#a3be8c"},
		{prs.AgentUnknown, "·", "#4c566a"},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			p := agentFixture(1, tc.status)
			p.Title, p.State = "Neutral title", "MERGED"
			p.GroupTiming = &prs.GroupTiming{Group: p.Group(), Since: "2026-01-01T00:00:00Z"}
			now := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
			layout := listRowLayout([]prs.PullRequest{p}, 150, now)
			for _, selected := range []bool{false, true} {
				out := layout.render(p, now, selected)
				plain := ansi.Strip(out)
				if layout.iconWidth != 2 || ansi.Cut(plain, 2, 4) != tc.icon+" " || !strings.Contains(plain, p.Title) {
					t.Fatalf("wrong dedicated icon/title slot: %+v %q", layout, plain)
				}
				pen := assertRenderedForeground(t, out, tc.icon, tc.color)
				if (pen.Bg != nil) != selected {
					t.Fatal("icon lost selection background")
				}
				assertRenderedForeground(t, out, "merged", "#b48ead")
				for _, token := range []string{p.Title, "1m", "repo #1↗", p.Branch} {
					if styleAt(t, out, token).Fg != nil {
						t.Fatalf("agent color leaked into %q", token)
					}
				}
				if selected && !strings.HasPrefix(plain, "› "+tc.icon+" ") {
					t.Fatal("selection marker replaced by status")
				}
			}
		})
	}
	legacy := agentFixture(1, prs.AgentNone)
	legacy.Links[0].Agent = true
	if got := ansi.Strip(listRowLayout([]prs.PullRequest{legacy}, 80, time.Time{}).render(legacy, time.Time{}, true)); !strings.HasPrefix(got, "› · ") {
		t.Fatal("legacy agent without status must be unknown: " + got)
	}
	lipgloss.SetColorProfile(termenv.Ascii)
	p := agentFixture(1, prs.AgentWorking)
	out := listRowLayout([]prs.PullRequest{p}, 80, time.Time{}).render(p, time.Time{}, true)
	if strings.Contains(out, "\x1b[") || !strings.HasPrefix(out, "› ● ") {
		t.Fatal("no-color must retain independent selection and native glyph: " + out)
	}
}

func TestAgentPresenceReservesSharedIconSlotAndFilteringReclaimsIt(t *testing.T) {
	p, other := agentFixture(1, prs.AgentDone), fixture(2)
	p.Title, other.Title = "Agent title", "Other title"
	rows := []prs.PullRequest{p, other}
	for _, width := range []int{1, 23, 24, 100} {
		layout := listRowLayout(rows, width, time.Time{})
		for i, row := range rows {
			out := ansi.Strip(layout.render(row, time.Time{}, true))
			if ansi.StringWidth(out) != width {
				t.Fatalf("icon overflow at %d: %q", width, out)
			}
			if width >= 24 {
				prefix := "› ● "
				if i == 1 {
					prefix = "›   "
				}
				if !strings.HasPrefix(out, prefix+row.Title) {
					t.Fatal("shared slot shifted title or invented presence: " + out)
				}
			} else if strings.Contains(out, "●") {
				t.Fatal("icon consumed tiny viewport title budget")
			}
		}
	}
	filtered := listRowLayout(rows[1:], 100, time.Time{})
	out := ansi.Strip(filtered.render(other, time.Time{}, true))
	if filtered.iconWidth != 0 || !strings.HasPrefix(out, "› Other title") {
		t.Fatal("filtered agent-free list did not reclaim slot: " + out)
	}
}

func TestAgentOverlayCatalogDetailErrorsAndOwnership(t *testing.T) {
	p := agentFixture(1, prs.AgentWorking)
	m := modelWith(t, Actions{AgentSnapshot: fakeAgentSnapshot, Detail: func(_ context.Context, row prs.PullRequest) (prs.Detail, error) {
		return prs.Detail{PR: p, Body: "live prose"}, nil
	}}, p)
	m.err = "Refresh: GitHub unavailable"
	initial := agentSnapshot(prs.AgentDone)
	initial.Agents = []prs.AgentInfo{{WorkspaceID: "workspace", TabID: "tab", PaneID: "pane", AgentStatus: prs.AgentDone}}
	initial.Tabs = []prs.Tab{{ID: "tab", Label: "original"}}
	initial.Workspaces = []prs.Workspace{{ID: "workspace", Worktree: &struct {
		CheckoutPath string `json:"checkout_path"`
	}{CheckoutPath: "/work/checkout"}}}
	acceptAgents(t, m, initial, nil)
	initial.Panes[0].AgentStatus = prs.AgentBlocked
	initial.Agents[0].AgentStatus = prs.AgentBlocked
	initial.Tabs[0].Label = "mutated"
	initial.Workspaces[0].ID = "mutated"
	initial.Workspaces[0].Worktree.CheckoutPath = "/mutated"
	if m.agentSnapshot.Panes[0].AgentStatus != prs.AgentDone || m.agentSnapshot.Agents[0].AgentStatus != prs.AgentDone || m.agentSnapshot.Tabs[0].Label != "original" || m.agentSnapshot.Workspaces[0].ID != "workspace" || m.agentSnapshot.Workspaces[0].Worktree.CheckoutPath != "/work/checkout" {
		t.Fatal("model retained caller-owned snapshot slices")
	}
	for _, cached := range []bool{true, false} {
		m.applyCatalog(prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{p}}, Cached: cached})
		if m.rows[0].AgentStatus() != prs.AgentDone || p.Links[0].AgentStatus != prs.AgentWorking {
			t.Fatal("late catalog clobbered latest status or mutated source")
		}
	}
	_, render := m.Update(press(m, tea.KeyCtrlD)())
	finishRender(t, m, render)
	if m.detail.PR.AgentStatus() != prs.AgentDone || !strings.Contains(plain(m), "live prose") {
		t.Fatal("live prose regressed latest agent metadata")
	}
	for _, flags := range []prs.DetailUpdate{{Cached: true}, {FromCache: true}} {
		flags.Detail = prs.Detail{PR: p, Body: "cached prose"}
		_, render = m.Update(detailUpdateMsg{token: m.detailToken, key: p.Key(), update: flags})
		finishRender(t, m, render)
		if m.detail.PR.AgentStatus() != prs.AgentDone || !strings.Contains(plain(m), "cached prose") {
			t.Fatal("disk prose regressed latest agent metadata")
		}
	}
	acceptAgents(t, m, prs.Snapshot{}, errors.New("sensitive socket path"))
	if m.rows[0].AgentStatus() != prs.AgentUnknown || m.detail.PR.AgentStatus() != prs.AgentUnknown || m.agentErr != "Herdr status unavailable" || strings.Contains(m.notices(), "sensitive") {
		t.Fatal("failure showed stale native state or leaked backend error")
	}
	m.applyCatalog(prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{p}}, Cached: true})
	if m.rows[0].AgentStatus() != prs.AgentUnknown {
		t.Fatal("cached catalog revived live-looking status during error")
	}
	m.err = "Refresh: GitHub unavailable"
	acceptAgents(t, m, agentSnapshot(prs.AgentIdle), nil)
	if m.agentErr != "" || m.err != "Refresh: GitHub unavailable" || m.rows[0].AgentStatus() != prs.AgentIdle {
		t.Fatal("successful snapshot failed recovery or cleared unrelated error")
	}
	// A new source in a later catalog must not borrow the old pane's agent.
	newPane := cloneRenderDetail(prs.Detail{PR: p}).PR
	newPane.Links[0].PaneID = "new-pane"
	m.applyCatalog(prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{newPane}}})
	if m.rows[0].AgentStatus() != prs.AgentNone {
		t.Fatal("new source inherited an old agent")
	}
	s := agentSnapshot(prs.AgentWorking)
	s.Panes[0].ID = "new-pane"
	acceptAgents(t, m, s, nil)
	if m.rows[0].AgentStatus() != prs.AgentWorking {
		t.Fatal("next snapshot did not catch up to new source")
	}
}

func TestAgentFilteredSelectionSyncsPreviewAndReusesCachedProse(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "debounced", true: "cached"}[cached], func(t *testing.T) {
			p, next, snapshot := agentPreviewPair()
			var loads, calls int
			m := wideModel(t, Actions{
				AgentSnapshot: fakeAgentSnapshot,
				Load: func(context.Context) (prs.Catalog, error) {
					loads++
					return prs.Catalog{PRs: []prs.PullRequest{p, next}}, nil
				},
				Detail: func(_ context.Context, row prs.PullRequest) (prs.Detail, error) {
					calls++
					body := "FIRST PROSE"
					if row.Key() == next.Key() {
						body = "SECOND PROSE"
					}
					return prs.Detail{PR: row, Body: body}, nil
				},
			}, p, next)
			_, render := m.Update(previewRequest(t, m)())
			finishRender(t, m, render)
			if cached {
				press(m, tea.KeyDown)
				_, render = m.Update(previewRequest(t, m)())
				finishRender(t, m, render)
				if press(m, tea.KeyUp) != nil || !strings.Contains(plain(m), "FIRST PROSE") || calls != 2 {
					t.Fatal("fixture did not retain both prose/render caches")
				}
			}
			typeQuery(m, "agent")
			oldToken, epoch, beforeCalls := m.detailToken, m.renderEpoch, calls
			old := *m.detail
			// Both rows retain presence: status alone cannot refetch/reparse or move selection.
			for i := range snapshot.Panes {
				snapshot.Panes[i].AgentStatus = prs.AgentDone
			}
			acceptAgents(t, m, snapshot, nil)
			if chosen(m) != p.Number || len(m.rows) != 2 || calls != beforeCalls || loads != 1 || m.detailToken != oldToken || m.renderEpoch != epoch || len(m.renderPending) != 0 || m.detail.PR.AgentStatus() != prs.AgentDone {
				t.Fatal("same-identity metadata update fetched/reparsed/reordered")
			}
			// Removing only the first agent filters out PR 1 and activates PR 2.
			snapshot.Panes = snapshot.Panes[1:]
			follow := acceptAgents(t, m, snapshot, nil)
			if chosen(m) != next.Number || len(m.rows) != 1 || m.detailKey != next.Key() || m.detailToken == oldToken || strings.Contains(plain(m), "FIRST PROSE") {
				t.Fatal("agent exit did not automatically activate replacement preview")
			}
			if cached {
				if m.detail == nil || m.detail.Body != "SECOND PROSE" || m.detail.PR.AgentStatus() != prs.AgentDone || m.detailLoading || calls != beforeCalls || m.renderEpoch != epoch+1 || len(m.renderPending) != 0 || !strings.Contains(plain(m), "SECOND PROSE") {
					t.Fatal("replacement failed to reuse cached prose/render without HTTP/Markdown")
				}
			} else {
				if m.detail != nil || !m.detailLoading || !m.detailDebouncing || calls != beforeCalls || follow == nil {
					t.Fatal("uncached replacement failed to debounce automatically")
				}
				// Execute the returned batch, not an assumed command index.
				h := &streamHarness{m: m, messages: make(chan tea.Msg, 128)}
				t.Cleanup(func() { m.cancel(); h.wg.Wait() })
				h.start(follow)
				h.until(t, func() bool { return m.detail != nil && len(m.renderPending) == 0 })
				if m.detail.PR.Key() != next.Key() || !strings.Contains(plain(m), "SECOND PROSE") || m.detailLoading || calls != beforeCalls+1 || loads != 1 {
					t.Fatal("replacement failed to fetch/render correct preview")
				}
			}
			old.Body = "LATE FIRST PROSE"
			for _, late := range []tea.Msg{
				previewMsg{token: oldToken, key: p.Key()},
				detailUpdateMsg{token: oldToken, key: p.Key(), update: prs.DetailUpdate{Detail: old}},
				detailMsg{token: oldToken, key: p.Key(), detail: old},
				detailMsg{token: oldToken, key: p.Key(), err: errors.New("late failure")},
			} {
				if _, cmd := m.Update(late); cmd != nil || m.detail.PR.Key() != next.Key() || m.detailErr != "" || m.detailCache[p.Key()].Body != "FIRST PROSE" {
					t.Fatal("old generation disturbed replacement preview/cache")
				}
			}
		})
	}
}

func TestAgentFilteredSelectionCancelsInflightPreview(t *testing.T) {
	p, next, snapshot := agentPreviewPair()
	started := make(chan context.Context, 1)
	results := make(chan tea.Msg, 1)
	var calls atomic.Int32
	m := wideModel(t, Actions{AgentSnapshot: fakeAgentSnapshot, Detail: func(ctx context.Context, row prs.PullRequest) (prs.Detail, error) {
		calls.Add(1)
		if row.Key() == p.Key() {
			started <- ctx
			<-ctx.Done()
			return prs.Detail{PR: row, Body: "CANCELED FIRST PROSE"}, nil
		}
		return prs.Detail{PR: row, Body: "SECOND PROSE"}, nil
	}}, p, next)
	typeQuery(m, "agent")
	token, epoch := m.detailToken, m.renderEpoch
	acceptAgents(t, m, snapshot, nil)
	if m.detail != nil || m.detailToken != token || m.renderEpoch != epoch || !m.detailDebouncing || calls.Load() != 0 {
		t.Fatal("metadata-only snapshot restarted pending debounce")
	}
	request := previewRequest(t, m)
	go func() { results <- request() }()
	var ctx context.Context
	select {
	case ctx = <-started:
	case <-time.After(time.Second):
		t.Fatal("first preview did not start")
	}
	token = m.detailToken
	acceptAgents(t, m, snapshot, nil)
	if ctx.Err() != nil || m.detailToken != token || m.renderEpoch != epoch || !m.detailLoading || m.detailDebouncing || calls.Load() != 1 {
		t.Fatal("metadata-only snapshot canceled/restarted inflight request")
	}
	snapshot.Panes = snapshot.Panes[1:]
	acceptAgents(t, m, snapshot, nil)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("filtered selection change did not cancel old request")
	}
	var late tea.Msg
	select {
	case late = <-results:
	case <-time.After(time.Second):
		t.Fatal("canceled request did not return")
	}
	m.Update(late)
	if m.detail != nil || !m.detailDebouncing || m.detailKey != next.Key() || len(m.detailCache) != 0 {
		t.Fatal("canceled response was accepted before replacement debounce")
	}
	_, render := m.Update(previewRequest(t, m)())
	finishRender(t, m, render)
	m.Update(late)
	if m.detail.PR.Key() != next.Key() || !strings.Contains(plain(m), "SECOND PROSE") || len(m.detailCache) != 1 || calls.Load() != 2 {
		t.Fatal("canceled response disturbed replacement preview/cache")
	}
}

func TestAgentFilteredSelectionFullscreenRemainsPinned(t *testing.T) {
	for _, survivor := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "replacement"}[survivor], func(t *testing.T) {
			p, next, snapshot := agentPreviewPair()
			p.GroupTiming = &prs.GroupTiming{Group: p.Group(), Since: "2026-01-01T00:00:00Z"}
			var calls int
			m := wideModel(t, Actions{AgentSnapshot: fakeAgentSnapshot, Detail: func(_ context.Context, row prs.PullRequest) (prs.Detail, error) {
				calls++
				return prs.Detail{PR: row, Body: strings.Repeat("PINNED PROSE\n\n", 30)}, nil
			}}, p, next)
			press(m, tea.KeyShiftTab)
			press(m, tea.KeyShiftTab)
			typeQuery(m, "agent")
			_, render := m.Update(previewRequest(t, m)())
			finishRender(t, m, render)
			finishRender(t, m, press(m, tea.KeyCtrlD))
			press(m, tea.KeyPgDown)
			scroll, token, epoch, clock := m.detailTop, m.detailToken, m.renderEpoch, m.clock
			for i := range snapshot.Panes {
				snapshot.Panes[i].AgentStatus = prs.AgentDone
			}
			acceptAgents(t, m, snapshot, nil)
			if chosen(m) != p.Number || m.detail.PR.AgentStatus() != prs.AgentDone || calls != 1 || m.renderEpoch != epoch || m.detailTop != scroll {
				t.Fatal("same-item native status reparsed/refetched or moved fullscreen")
			}
			if survivor {
				snapshot.Panes = snapshot.Panes[1:]
			} else {
				snapshot = prs.Snapshot{}
			}
			acceptAgents(t, m, snapshot, nil)
			if survivor && chosen(m) != next.Number || !survivor && len(m.rows) != 0 || !m.inDetail || m.detailKey != p.Key() || m.detail.PR.AgentStatus() != prs.AgentNone || m.detailTop != scroll || scroll == 0 || m.detailToken != token || m.renderEpoch != epoch || calls != 1 || !strings.Contains(plain(m), "PINNED PROSE") {
				t.Fatal("agent exit lost fullscreen pin/prose/scroll or fetched replacement")
			}
			if m.query != "agent" || m.groupFilter != p.Group() || m.clock != clock || m.detail.PR.GroupTiming.Since != p.GroupTiming.Since {
				t.Fatal("native status changed query/group/group clock")
			}
			acceptAgents(t, m, agentSnapshot(prs.AgentIdle), nil)
			if chosen(m) != p.Number || m.detail.PR.AgentStatus() != prs.AgentIdle || calls != 1 || m.renderEpoch != epoch || !strings.Contains(plain(m), "PINNED PROSE") {
				t.Fatal("agent reappearance failed to restore searchable presence without HTTP/Markdown")
			}
		})
	}
}

func TestNativeAgentStatusCannotOverrideCurrentCheckoutNavigation(t *testing.T) {
	for _, status := range []prs.AgentStatus{prs.AgentBlocked, prs.AgentWorking, prs.AgentDone, prs.AgentIdle} {
		t.Run(string(status), func(t *testing.T) {
			peer, here := agentFixture(1, status), agentFixture(2, prs.AgentIdle)
			here.Links[0].Current = true
			m := modelWith(t, Actions{AgentSnapshot: fakeAgentSnapshot}, peer, here)
			if chosen(m) != here.Number || m.rows[0].Number != here.Number || !strings.Contains(plain(m), "#2↗*") {
				t.Fatal("native status displaced current checkout's initial navigation")
			}
			press(m, tea.KeyDown)
			acceptAgents(t, m, agentSnapshot(prs.AgentDone), nil)
			if chosen(m) != peer.Number || m.rows[0].Number != here.Number || m.rows[1].Group() != prs.GroupInReview || !strings.Contains(plain(m), peer.Title) {
				t.Fatal("unread native Done reordered selection or invented merged lifecycle")
			}
		})
	}
}

func TestAgentPollPreservesListOrderSelectionViewportAndInitialFailure(t *testing.T) {
	var rows []prs.PullRequest
	for i := 1; i <= 18; i++ {
		rows = append(rows, agentFixture(i, prs.AgentDone))
	}
	m := modelWith(t, Actions{AgentSnapshot: fakeAgentSnapshot}, rows...)
	m.height = 12
	press(m, tea.KeyEnd)
	selected, _ := m.selected()
	top, cursor, clock := m.top, m.cursor, m.clock
	keys := make([]string, len(m.rows))
	for i, p := range m.rows {
		keys[i] = p.Key()
	}
	// Initial errors have no earlier snapshot to overlay, but must not leave
	// catalog-provided Done icons looking like a live unread observation.
	acceptAgents(t, m, prs.Snapshot{}, errors.New("offline"))
	if m.rows[0].AgentStatus() != prs.AgentUnknown || !m.rows[0].Links[0].Agent {
		t.Fatal("initial failure retained unread state or erased known presence")
	}
	acceptAgents(t, m, agentSnapshot(prs.AgentBlocked), nil)
	p, _ := m.selected()
	if p.Key() != selected.Key() || m.cursor != cursor || m.top != top || m.clock != clock || m.agentErr != "" || m.query != "" || m.groupFilter != "" {
		t.Fatal("metadata poll reset list navigation/clock")
	}
	for i, p := range m.rows {
		if p.Key() != keys[i] || p.AgentStatus() != prs.AgentBlocked {
			t.Fatal("agent status changed group rank or failed to refresh all rows")
		}
	}
}

func TestAgentPollingLifecycleCacheFirstSingleCycleAndCancellation(t *testing.T) {
	var loads, details, snapshots atomic.Int32
	p := agentFixture(1, prs.AgentIdle)
	started := make(chan context.Context, 1)
	responses := make(chan prs.Snapshot, 1)
	h := newStreamHarness(t, Actions{
		Load: func(context.Context) (prs.Catalog, error) {
			loads.Add(1)
			return prs.Catalog{PRs: []prs.PullRequest{p}}, nil
		},
		Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
			details.Add(1)
			return prs.Detail{PR: p, Body: "prose"}, nil
		},
		AgentSnapshot: func(ctx context.Context) (prs.Snapshot, error) {
			snapshots.Add(1)
			started <- ctx
			select {
			case <-ctx.Done():
				return prs.Snapshot{}, ctx.Err()
			case s := <-responses:
				return s, nil
			}
		},
	})
	h.start(h.m.Init())
	if h.m.Init() != nil || h.m.pollAgents() != nil {
		t.Fatal("duplicate Init/poll reserved another cycle")
	}
	h.until(t, func() bool { return h.m.detail != nil && len(h.m.renderPending) == 0 })
	epoch := h.m.renderEpoch
	if snapshots.Load() > 1 || loads.Load() != 1 || details.Load() != 1 || h.m.rows[0].AgentStatus() != prs.AgentIdle || !strings.Contains(plain(h.m), "prose") {
		t.Fatal("polling blocked cache-first paint or duplicated backend work")
	}
	var request context.Context
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case request = <-started:
	case <-timer.C:
		t.Fatal("snapshot not started")
	}
	deadline, ok := request.Deadline()
	if !ok || time.Until(deadline) > 5*time.Second {
		t.Fatal("snapshot request not bounded")
	}
	h.m.Update(clockMsg(time.Now())) // Age ticks never start another snapshot.
	if h.m.pollAgents() != nil || snapshots.Load() != 1 {
		t.Fatal("overlapping snapshot request")
	}
	responses <- agentSnapshot(prs.AgentDone)
	h.until(t, func() bool { return h.m.rows[0].AgentStatus() == prs.AgentDone })
	if snapshots.Load() != 1 || loads.Load() != 1 || details.Load() != 1 || h.m.renderEpoch != epoch || !strings.Contains(plain(h.m), "prose") {
		t.Fatal("one readonly cycle performed Git/GitHub/Markdown work or hid prose")
	}
	select {
	case request = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("next cycle not scheduled")
	}
	if snapshots.Load() != 2 || h.m.pollAgents() != nil {
		t.Fatal("next cycle duplicated")
	}
	oldToken := h.m.agentToken
	press(h.m, tea.KeyCtrlC)
	select {
	case <-request.Done():
	case <-time.After(time.Second):
		t.Fatal("quit did not cancel snapshot")
	}
	h.m.Update(agentSnapshotMsg{oldToken, agentSnapshot(prs.AgentBlocked), nil})
	if h.m.rows[0].AgentStatus() != prs.AgentDone || h.m.pollAgents() != nil || snapshots.Load() != 2 {
		t.Fatal("late result or quit restarted polling")
	}
}

func TestAgentPollingDisabledAndCancelledTimer(t *testing.T) {
	m := modelWith(t, Actions{}, fixture(1))
	if m.pollAgents() != nil || m.agentPolling {
		t.Fatal("nil action enabled polling")
	}
	var calls atomic.Int32
	m = modelWith(t, Actions{AgentSnapshot: func(context.Context) (prs.Snapshot, error) { calls.Add(1); return prs.Snapshot{}, nil }}, fixture(1))
	cmd := m.pollAgents()
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	m.cancel()
	select {
	case msg := <-done:
		if msg != nil {
			t.Fatal("cancelled timer delivered a result")
		}
	case <-time.After(time.Second):
		t.Fatal("timer not cancellation bound")
	}
	if calls.Load() != 0 || m.pollAgents() != nil {
		t.Fatal("cancelled timer called Herdr")
	}
}

func TestAgentPollingRejectsSupersededResultsAndNoErrorSpin(t *testing.T) {
	var calls atomic.Int32
	m := modelWith(t, Actions{AgentSnapshot: func(context.Context) (prs.Snapshot, error) {
		calls.Add(1)
		return prs.Snapshot{}, errors.New("offline")
	}}, agentFixture(1, prs.AgentWorking))
	m.pollAgents()
	m.Update(agentSnapshotMsg{m.agentToken + 1, agentSnapshot(prs.AgentDone), nil})
	if m.rows[0].AgentStatus() != prs.AgentWorking || !m.agentPolling {
		t.Fatal("stale token accepted")
	}
	cmd := acceptAgents(t, m, prs.Snapshot{}, errors.New("offline"))
	if cmd == nil || !m.agentPolling || m.agentToken != 2 {
		t.Fatal("error did not reserve exactly one delayed retry")
	}
	m.Update(agentSnapshotMsg{1, agentSnapshot(prs.AgentDone), nil})
	if m.rows[0].AgentStatus() != prs.AgentUnknown || m.agentErr == "" {
		t.Fatal("late success cleared newer failure")
	}
	// Execute retry off the UI thread; cancellation must stop its delay without calling Herdr.
	m.cancel()
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retry timer did not stop")
	}
	if calls.Load() != 0 {
		t.Fatal("error/retry spin called Herdr before the delayed command")
	}
}

func TestShiftTabFooterLabelEveryMode(t *testing.T) {
	m := modelWith(t, Actions{}, fixture(1))
	for _, mode := range []struct {
		width         int
		full, focused bool
	}{{80, false, false}, {120, false, false}, {120, false, true}, {120, true, false}} {
		m.width, m.inDetail, m.previewFocused = mode.width, mode.full, mode.focused
		footer := m.footer()
		if !strings.Contains(footer, "⇧Tab groups") || strings.Contains(footer, "Shift") {
			t.Fatal("incorrect visible key label: " + footer)
		}
		for width := 1; width <= 200; width++ {
			m.width = width
			footer = m.footer()
			out := plain(m)
			if strings.Contains(out, "ShiftTab") {
				t.Fatal("old visible label")
			}
			before, _, _ := strings.Cut(footer, "⇧Tab groups")
			if width > ansi.StringWidth(before+"⇧Tab groups") && !strings.Contains(out, "⇧Tab groups") {
				t.Fatalf("footer hint missing at %d", width)
			}
		}
	}
}
