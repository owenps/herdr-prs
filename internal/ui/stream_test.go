package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/owenps/herdr-prs/internal/prs"
)

// Run commands concurrently, but deliver every message on the test/UI thread.
// This exercises the actual receiver pipeline, including tea.Batch fan-out.
type streamHarness struct {
	m        *Model
	messages chan tea.Msg
	wg       sync.WaitGroup
}

func newStreamHarness(t *testing.T, actions Actions) *streamHarness {
	t.Helper()
	h := &streamHarness{m: NewModel(context.Background(), actions), messages: make(chan tea.Msg, 128)}
	h.m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	t.Cleanup(func() {
		h.m.cancel()
		done := make(chan struct{})
		go func() { h.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("stream commands did not stop on cancellation")
		}
	})
	return h
}

func (h *streamHarness) start(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	h.wg.Add(1)
	ctx := h.m.ctx
	go func() {
		defer h.wg.Done()
		msg := cmd()
		select {
		case h.messages <- msg:
		case <-ctx.Done():
		}
	}()
}

func (h *streamHarness) deliver(msg tea.Msg) {
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, cmd := range batch {
			h.start(cmd)
		}
		return
	}
	_, cmd := h.m.Update(msg)
	h.start(cmd)
}

func (h *streamHarness) until(t *testing.T, ready func() bool) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for !ready() {
		select {
		case msg := <-h.messages:
			h.deliver(msg)
		case <-timer.C:
			t.Fatalf("stream stalled: %s", plain(h.m))
		}
	}
}

func requireFlag(t *testing.T, flags <-chan bool, want bool) {
	t.Helper()
	select {
	case got := <-flags:
		if got != want {
			t.Fatalf("force=%v, want %v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("backend not called")
	}
}

func TestStreamCachedPreviewBeforeCatalogCompletes(t *testing.T) {
	p, other := fixture(1), fixture(2)
	p.MergeState, p.UpdatedAt, p.HeadOID = "CLEAN", "revision", "head"
	p.Links = []prs.Link{{Current: true, WorkspaceID: "local"}}
	catalogGate, detailGate := make(chan struct{}), make(chan struct{})
	loadFlags, detailFlags := make(chan bool, 4), make(chan bool, 4)
	h := newStreamHarness(t, Actions{
		LoadStream: func(ctx context.Context, force bool, emit func(prs.CatalogUpdate)) error {
			loadFlags <- force
			emit(prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{p, other}, Warnings: []string{"cached metadata"}}, Cached: true})
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-catalogGate:
			}
			fresh := p
			fresh.Unresolved = 1
			emit(prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{fresh, other}}})
			return nil
		},
		DetailStream: func(ctx context.Context, selected prs.PullRequest, force bool, emit func(prs.DetailUpdate)) error {
			detailFlags <- force
			old := selected
			old.Repository, old.Number, old.Unresolved = "wrong/repository", 999, 1
			emit(prs.DetailUpdate{Detail: prs.Detail{PR: old, Body: "cached body"}, Cached: true})
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-detailGate:
			}
			selected.Unresolved = 1
			emit(prs.DetailUpdate{Detail: prs.Detail{PR: selected, Body: "fresh body"}})
			return nil
		},
	})
	h.start(h.m.Init())
	h.until(t, func() bool { return h.m.detail != nil && len(h.m.renderPending) == 0 })
	requireFlag(t, loadFlags, false)
	requireFlag(t, detailFlags, false)
	if !h.m.loading || !h.m.detailLoading || !h.m.stale || !h.m.detailStale || h.m.rows[0].Status() != "ready" || h.m.detail.PR.Status() != "ready" {
		t.Fatal("cached detail blocked or regressed newer catalog status")
	}
	if h.m.detail.PR.Key() != p.Key() || len(h.m.detail.PR.Links) != 1 || !strings.Contains(plain(h.m), "cached body") || !strings.Contains(plain(h.m), "cached metadata") {
		t.Fatal(plain(h.m))
	}
	close(detailGate)
	h.until(t, func() bool { return !h.m.detailLoading && len(h.m.renderPending) == 0 })
	if !strings.Contains(plain(h.m), "fresh body") || h.m.detail.Body != "fresh body" || h.m.rows[0].Status() != "feedback" || h.m.detailStale || !h.m.loading {
		t.Fatal("fresh detail did not apply while catalog remained pending")
	}
	token := h.m.detailToken
	close(catalogGate)
	h.until(t, func() bool { return !h.m.loading })
	if h.m.stale || h.m.detailToken != token || h.m.detail.Body != "fresh body" {
		t.Fatal("unchanged final catalog cleared/refetched preview")
	}
	select {
	case <-detailFlags:
		t.Fatal("duplicate detail fetch")
	default:
	}
}

func TestStreamForceBothTTLsAndUnchangedCatalogDoesNotCancelDetail(t *testing.T) {
	p := fixture(1)
	p.UpdatedAt, p.HeadOID = "revision", "head"
	loadFlags, detailFlags := make(chan bool, 4), make(chan bool, 4)
	catalogGate, detailGate := make(chan struct{}), make(chan struct{})
	var detailCalls atomic.Int32
	h := newStreamHarness(t, Actions{
		LoadStream: func(ctx context.Context, force bool, emit func(prs.CatalogUpdate)) error {
			loadFlags <- force
			emit(prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{p}}, Cached: force})
			if force {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-catalogGate:
				}
			}
			emit(prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{p}}})
			return nil
		},
		DetailStream: func(ctx context.Context, selected prs.PullRequest, force bool, emit func(prs.DetailUpdate)) error {
			detailCalls.Add(1)
			detailFlags <- force
			emit(prs.DetailUpdate{Detail: prs.Detail{PR: selected, Body: "TTL body"}, Cached: force})
			if force {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-detailGate:
				}
				emit(prs.DetailUpdate{Detail: prs.Detail{PR: selected, Body: "forced body"}})
			}
			return nil
		},
	})
	h.start(h.m.Init())
	h.until(t, func() bool { return h.m.detail != nil && !h.m.detailLoading && !h.m.loading })
	requireFlag(t, loadFlags, false)
	requireFlag(t, detailFlags, false)
	if h.m.stale || h.m.detailStale {
		t.Fatal("fresh TTL cache incorrectly marked stale")
	}
	h.deliver(tea.KeyMsg{Type: tea.KeyCtrlR})
	h.until(t, func() bool { return detailCalls.Load() == 2 && h.m.detailStale })
	requireFlag(t, loadFlags, true)
	requireFlag(t, detailFlags, true)
	token := h.m.detailToken
	close(catalogGate)
	h.until(t, func() bool { return !h.m.loading })
	if !h.m.detailLoading || h.m.detailToken != token || detailCalls.Load() != 2 {
		t.Fatal("catalog stage canceled forced detail")
	}
	close(detailGate)
	h.until(t, func() bool { return !h.m.detailLoading })
	if h.m.detail.Body != "forced body" || h.m.detailStale || h.m.detailErr != "" {
		t.Fatal("forced completion lost snapshot")
	}
}

func TestStreamSelectionPreservedAcrossStagesAndFreshness(t *testing.T) {
	first, here, third := fixture(1), fixture(2), fixture(3)
	here.Links = []prs.Link{{Current: true}}
	updates := make(chan prs.CatalogUpdate, 1)
	var calls atomic.Int32
	h := newStreamHarness(t, Actions{
		LoadStream: func(ctx context.Context, _ bool, emit func(prs.CatalogUpdate)) error {
			emit(prs.CatalogUpdate{Cached: true})
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case update := <-updates:
					emit(update)
				}
			}
		},
		Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
			calls.Add(1)
			return prs.Detail{PR: p, Body: "prose " + p.UpdatedAt + " / " + p.HeadOID}, nil
		},
	})
	h.start(h.m.Init())
	h.until(t, func() bool { return h.m.loaded })
	if len(h.m.rows) != 0 {
		t.Fatal("empty first stage invented selection")
	}
	updates <- prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{first, here}}, Cached: true}
	h.until(t, func() bool { return h.m.detail != nil && len(h.m.renderPending) == 0 })
	if chosen(h.m) != 2 || !strings.Contains(plain(h.m), "prose") {
		t.Fatal("first nonempty stage did not preview current checkout")
	}
	h.deliver(tea.KeyMsg{Type: tea.KeyDown})
	h.until(t, func() bool { return h.m.detail != nil && h.m.detail.PR.Number == 1 && len(h.m.renderPending) == 0 })
	third.Unresolved = 1
	updates <- prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{first, here, third}}}
	h.until(t, func() bool { return len(h.m.rows) == 3 })
	if chosen(h.m) != 1 || calls.Load() != 2 || !strings.Contains(plain(h.m), "prose") {
		t.Fatal("metadata stage jumped selection or refetched unchanged preview")
	}
	for _, revision := range []string{"updatedAt", "headOID"} {
		if revision == "updatedAt" {
			first.UpdatedAt = "new revision"
		} else {
			first.HeadOID = "new head"
		}
		before := calls.Load()
		updates <- prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{first, here, third}}}
		h.until(t, func() bool {
			return h.m.detail != nil && h.m.detail.PR.UpdatedAt == first.UpdatedAt && h.m.detail.PR.HeadOID == first.HeadOID && h.m.detail.Body == "prose "+first.UpdatedAt+" / "+first.HeadOID && len(h.m.renderPending) == 0
		})
		if chosen(h.m) != 1 || calls.Load() != before+1 || !strings.Contains(plain(h.m), first.UpdatedAt) || !strings.Contains(plain(h.m), first.HeadOID) {
			t.Fatal(revision + " did not fetch/display the selected revision")
		}
	}
}

func TestStreamErrorsRetainSnapshotsAndSeparateStaleState(t *testing.T) {
	p := fixture(1)
	catalogGate, detailGate := make(chan struct{}), make(chan struct{})
	h := newStreamHarness(t, Actions{
		LoadStream: func(ctx context.Context, _ bool, emit func(prs.CatalogUpdate)) error {
			emit(prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{p}}, Cached: true})
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-catalogGate:
			}
			return errors.New("catalog offline")
		},
		DetailStream: func(ctx context.Context, p prs.PullRequest, _ bool, emit func(prs.DetailUpdate)) error {
			emit(prs.DetailUpdate{Detail: prs.Detail{PR: p, Body: "retained body"}, Cached: true})
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-detailGate:
			}
			return errors.New("detail offline")
		},
	})
	h.start(h.m.Init())
	h.until(t, func() bool { return h.m.detail != nil && len(h.m.renderPending) == 0 })
	if !strings.Contains(plain(h.m), "retained body") {
		t.Fatal("cached body never painted")
	}
	close(catalogGate)
	close(detailGate)
	h.until(t, func() bool { return !h.m.loading && !h.m.detailLoading })
	if len(h.m.rows) != 1 || h.m.detail.Body != "retained body" || !h.m.stale || !h.m.detailStale || !strings.Contains(h.m.err, "catalog offline") || !strings.Contains(h.m.detailErr, "detail offline") {
		t.Fatal(plain(h.m))
	}
	// A fresh catalog cannot erase the independent detail error/stale flag.
	h.deliver(catalogUpdateMsg{token: h.m.loadToken, update: prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{p}}}})
	if h.m.err != "" || h.m.stale || !h.m.detailStale || h.m.detailErr == "" {
		t.Fatal("catalog success hid detail failure")
	}
}

func TestStreamsWithNoSnapshotsCompleteWithoutResettingData(t *testing.T) {
	h := newStreamHarness(t, Actions{
		LoadStream:   func(context.Context, bool, func(prs.CatalogUpdate)) error { return nil },
		DetailStream: func(context.Context, prs.PullRequest, bool, func(prs.DetailUpdate)) error { return nil },
	})
	h.start(h.m.Init())
	h.until(t, func() bool { return !h.m.loading })
	if h.m.loaded || h.m.err != "" {
		t.Fatal("empty stream invented a catalog or error")
	}
	p := fixture(1)
	h.m.actions.LoadStream = func(_ context.Context, _ bool, emit func(prs.CatalogUpdate)) error {
		emit(prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{p}}})
		return nil
	}
	h.deliver(tea.KeyMsg{Type: tea.KeyCtrlR})
	h.until(t, func() bool { return len(h.m.rows) == 1 && !h.m.loading && !h.m.detailLoading })
	if h.m.detail != nil || h.m.detailErr != "" || !strings.Contains(plain(h.m), "Details unavailable") {
		t.Fatal("empty detail stream invented a result or error")
	}
	// First accept and paint real data; only then can retention be tested.
	h.m.actions.DetailStream = func(_ context.Context, selected prs.PullRequest, _ bool, emit func(prs.DetailUpdate)) error {
		emit(prs.DetailUpdate{Detail: prs.Detail{PR: selected, Body: "keep me"}})
		return nil
	}
	h.deliver(tea.KeyMsg{Type: tea.KeyCtrlR})
	h.until(t, func() bool {
		return h.m.detail != nil && !h.m.loading && !h.m.detailLoading && len(h.m.renderPending) == 0
	})
	if !strings.Contains(plain(h.m), "keep me") {
		t.Fatal("fixture body never painted")
	}
	h.m.actions.LoadStream = func(context.Context, bool, func(prs.CatalogUpdate)) error { return nil }
	h.m.actions.DetailStream = func(context.Context, prs.PullRequest, bool, func(prs.DetailUpdate)) error { return nil }
	h.deliver(tea.KeyMsg{Type: tea.KeyCtrlR})
	h.until(t, func() bool { return !h.m.loading && !h.m.detailLoading })
	if len(h.m.rows) != 1 || !strings.Contains(plain(h.m), "keep me") || h.m.detailErr != "" {
		t.Fatal("completion reset accepted data")
	}
}

func TestStreamCancellationGenerationRaces(t *testing.T) {
	cancelled := make(chan string, 8)
	var calls atomic.Int32
	h := newStreamHarness(t, Actions{
		Load: func(context.Context) (prs.Catalog, error) {
			return prs.Catalog{PRs: []prs.PullRequest{fixture(1), fixture(2)}}, nil
		},
		DetailStream: func(ctx context.Context, p prs.PullRequest, _ bool, emit func(prs.DetailUpdate)) error {
			id := calls.Add(1)
			emit(prs.DetailUpdate{Detail: prs.Detail{PR: p, Body: fmt.Sprintf("cached-%d", id)}, Cached: true})
			<-ctx.Done()
			cancelled <- p.Key()
			// Even a backend that emits success after cancellation cannot win.
			emit(prs.DetailUpdate{Detail: prs.Detail{PR: p, Body: "late callback"}})
			return errors.New("late error")
		},
	})
	h.start(h.m.Init())
	h.until(t, func() bool { return h.m.detail != nil })
	oldToken := h.m.detailToken
	h.deliver(tea.KeyMsg{Type: tea.KeyDown})
	h.until(t, func() bool { return h.m.detail != nil && h.m.detail.PR.Number == 2 })
	h.deliver(tea.KeyMsg{Type: tea.KeyUp})
	h.until(t, func() bool { return h.m.detail != nil && h.m.detail.PR.Number == 1 })
	body := h.m.detail.Body
	for _, msg := range []tea.Msg{
		detailUpdateMsg{token: oldToken, key: fixture(1).Key(), update: prs.DetailUpdate{Detail: prs.Detail{PR: fixture(1), Body: "queued old"}}},
		detailMsg{token: oldToken, key: fixture(1).Key(), completeOnly: true, err: errors.New("queued old error")},
	} {
		h.deliver(msg)
	}
	if h.m.detail.Body != body || h.m.detailErr != "" || !h.m.detailLoading || len(h.m.detailCache) != 0 {
		t.Fatal("same-key superseded generation accepted")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatal("old request not canceled")
		}
	}
	// Fullscreen reuses the current receiver; only explicit refresh retries.
	token := h.m.detailToken
	h.deliver(tea.KeyMsg{Type: tea.KeyCtrlD})
	if calls.Load() != 3 || h.m.detailToken != token || !h.m.inDetail {
		t.Fatal("fullscreen refetched or replaced active receiver")
	}
	h.deliver(tea.KeyMsg{Type: tea.KeyCtrlR})
	h.until(t, func() bool { return calls.Load() == 4 })
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("explicit refresh did not cancel receiver")
	}
	// Assert cancellation before quit, which would otherwise mask a leak.
	h.deliver(tea.KeyMsg{Type: tea.KeyEsc})
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("back did not cancel fullscreen receiver")
	}
	h.until(t, func() bool { return calls.Load() == 5 && h.m.detail != nil })
	h.deliver(tea.WindowSizeMsg{Width: 80, Height: 24})
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("hidden preview did not cancel receiver")
	}
	if h.m.detail != nil || strings.Contains(plain(h.m), "cached-") || h.m.ctx.Err() != nil {
		t.Fatal("hidden preview retained body or cancelled the whole popup")
	}
	h.deliver(tea.KeyMsg{Type: tea.KeyCtrlC})
	if h.m.ctx.Err() != context.Canceled {
		t.Fatal("quit did not cancel streams")
	}
}

func TestStreamSupersededCatalogDoesNotMutateModel(t *testing.T) {
	started, cancelled := make(chan struct{}, 2), make(chan struct{}, 2)
	m := NewModel(context.Background(), Actions{LoadStream: func(ctx context.Context, _ bool, emit func(prs.CatalogUpdate)) error {
		started <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		emit(prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{fixture(99)}}})
		return errors.New("late failure")
	}})
	t.Cleanup(m.cancel)
	results := make(chan tea.Msg, 2)
	old := m.load(false)
	token := m.loadToken
	go func() { results <- old() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("catalog command did not start")
	}
	fresh := m.load(true)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("catalog not canceled")
	}
	select {
	case msg := <-results:
		m.Update(msg)
	case <-time.After(time.Second):
		t.Fatal("cancelled catalog receiver did not return")
	}
	m.Update(catalogUpdateMsg{token: token, update: prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{fixture(99)}}}})
	m.Update(loadMsg{token: token, completeOnly: true, err: errors.New("late error")})
	if !m.loading || len(m.rows) != 0 || m.err != "" {
		t.Fatal("superseded catalog stage accepted")
	}
	// Cancel before execution: no backend call, no model mutation.
	m.cancel()
	if fresh() != nil {
		t.Fatal("canceled stream started")
	}
}
