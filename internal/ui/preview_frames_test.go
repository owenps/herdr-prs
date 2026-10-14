package ui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

// Controlled provider stages; requests and rendering still use returned commands.
func framePreview(t *testing.T) (*streamHarness, prs.Detail, chan prs.DetailUpdate, chan prs.CatalogUpdate) {
	t.Helper()
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	p := fixture(1)
	d := prs.Detail{PR: p, Body: "# Old styled heading\n\n" + strings.Repeat("paragraph\n\n", 25)}
	details, catalogs := make(chan prs.DetailUpdate, 1), make(chan prs.CatalogUpdate, 1)
	details <- prs.DetailUpdate{Detail: d, Cached: true}
	h := newStreamHarness(t, Actions{
		LoadStream: func(ctx context.Context, force bool, emit func(prs.CatalogUpdate)) error {
			if !force {
				emit(prs.CatalogUpdate{Catalog: prs.Catalog{PRs: []prs.PullRequest{p, fixture(2)}}, Cached: true})
			}
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case update := <-catalogs:
					emit(update)
				}
			}
		},
		DetailStream: func(ctx context.Context, _ prs.PullRequest, _ bool, emit func(prs.DetailUpdate)) error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case update := <-details:
					emit(update)
				}
			}
		},
	})
	h.m.markdownProfile = termenv.TrueColor
	h.start(h.m.Init())
	h.until(t, func() bool { return h.m.detail != nil && len(h.m.renderPending) == 0 })
	assertMDColor(t, strings.Join(h.m.detailLines(), "\n"), "Old styled heading", mdGold)
	return h, d, details, catalogs
}

// Hold a genuine completed render while newer input invalidates it.
func nextRenderedFrame(t *testing.T, h *streamHarness) renderedDetailMsg {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case msg := <-h.messages:
			if frame, ok := msg.(renderedDetailMsg); ok {
				return frame
			}
			h.deliver(msg)
		case <-timer.C:
			t.Fatal("render command did not return")
		}
	}
}

func TestInitialPreviewPublishesStyledBodyOnce(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	m := wideModel(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		return prs.Detail{PR: p, Body: "# First styled heading"}, nil
	}}, fixture(1))
	m.markdownProfile = termenv.TrueColor
	_, render := m.Update(previewRequest(t, m)())
	if render == nil || strings.Contains(plain(m), "First styled heading") || !strings.Contains(m.notices(), "Loading") {
		t.Fatal("first body flashed plaintext or hid rendering progress")
	}
	finishRender(t, m, render)
	assertMDColor(t, strings.Join(m.detailLines(), "\n"), "First styled heading", mdGold)
	if strings.Contains(m.notices(), "Loading") {
		t.Fatal("rendering notice survived completed request")
	}
}

func plainDetail(m *Model) string { return ansi.Strip(strings.Join(m.detailLines(), "\n")) }

func TestIdenticalCachedLiveAndCatalogStagesReuseRenderedProse(t *testing.T) {
	h, d, details, catalogs := framePreview(t)
	m := h.m
	h.deliver(tea.KeyMsg{Type: tea.KeyTab})
	for range 5 {
		h.deliver(tea.KeyMsg{Type: tea.KeyDown})
	}
	frame, epoch := slices.Clone(m.detailLines()), m.renderEpoch
	details <- prs.DetailUpdate{Detail: d}
	h.until(t, func() bool { return !m.detailStale })
	catalogs <- prs.CatalogUpdate{Catalog: cloneCatalog(m.catalog)}
	h.until(t, func() bool { return !m.stale })
	if m.renderEpoch != epoch || !slices.Equal(frame, m.detailLines()) || m.detailTop != 5 {
		t.Fatal("identical provider stages reparsed/flickered/scrolled")
	}
	fresh := cloneCatalog(m.catalog)
	fresh.PRs[0].MergeState = "CLEAN"
	catalogs <- prs.CatalogUpdate{Catalog: fresh}
	h.until(t, func() bool { return m.detail.PR.Status() == "ready" })
	if m.renderEpoch != epoch || !strings.Contains(plainDetail(m), "ready") || !strings.Contains(plainDetail(m), "Old styled heading") {
		t.Fatalf("header-only status update: epoch=%d want=%d detail=%s", m.renderEpoch, epoch, plainDetail(m))
	}
	details <- prs.DetailUpdate{Detail: d, Cached: true}
	h.until(t, func() bool { return m.detailStale })
	if m.renderEpoch != epoch || m.detail.PR.Status() != "ready" || m.detailTop != 5 {
		t.Fatal("old cached prose regressed newer metadata or scroll")
	}
	// Navigation rebinds cached prose to current catalog metadata, without I/O.
	press(m, tea.KeyTab)
	press(m, tea.KeyDown)
	if cmd := press(m, tea.KeyUp); cmd != nil || m.detail.PR.Status() != "ready" || !strings.Contains(plain(m), "Old styled heading") {
		t.Fatal("selection cache lost authoritative status/rendered prose")
	}
}

func TestSamePRProseSwapsAtomicallyWithoutScrollLoss(t *testing.T) {
	h, d, details, _ := framePreview(t)
	m := h.m
	press(m, tea.KeyTab)
	for range 7 {
		press(m, tea.KeyDown)
	}
	frame := slices.Clone(m.detailLines())
	changed := d
	changed.Body = "# New styled heading\n\n" + strings.Repeat("new paragraph\n\n", 25)
	details <- prs.DetailUpdate{Detail: changed}
	h.until(t, func() bool { return m.detail.Body == changed.Body })
	if !slices.Equal(frame, m.detailLines()) || m.detailTop != 7 || strings.Contains(plainDetail(m), "New styled heading") {
		t.Fatal("pending same-PR prose lost complete styled frame/scroll")
	}
	h.until(t, func() bool { return len(m.renderPending) == 0 })
	assertMDColor(t, strings.Join(m.detailLines(), "\n"), "New styled heading", mdGold)
	if strings.Contains(plainDetail(m), "Old styled heading") || m.detailTop != 7 {
		t.Fatal("atomic replacement failed")
	}
}

func TestExplicitRefreshRejectsCompletedOldRenderAndReusesIdenticalFrame(t *testing.T) {
	h, d, details, _ := framePreview(t)
	m := h.m
	press(m, tea.KeyTab)
	for range 7 {
		press(m, tea.KeyDown)
	}
	frame := slices.Clone(m.detailLines())
	obsolete := d
	obsolete.Body = "# Obsolete refresh body"
	details <- prs.DetailUpdate{Detail: obsolete}
	h.until(t, func() bool { return m.detail.Body == obsolete.Body })
	late := nextRenderedFrame(t, h)
	if !slices.Equal(frame, m.detailLines()) {
		t.Fatal("render command mutated model before delivery")
	}
	h.deliver(tea.KeyMsg{Type: tea.KeyCtrlR})
	h.deliver(late)
	if !slices.Equal(frame, m.detailLines()) || m.detailTop != 7 || strings.Contains(plainDetail(m), "Obsolete refresh body") {
		t.Fatal("late render replaced retained refresh frame")
	}
	epoch := m.renderEpoch
	h.until(t, func() bool { return m.detailLoading && !m.detailDebouncing })
	details <- prs.DetailUpdate{Detail: d}
	h.until(t, func() bool { return m.detail.Body == d.Body })
	if m.renderEpoch != epoch || !slices.Equal(frame, m.detailLines()) || m.detailTop != 7 {
		t.Fatal("identical forced response reparsed/scrolled complete front buffer")
	}
}

func TestPreviewFrameRejectsCompletedRenderAfterInvalidation(t *testing.T) {
	for _, reason := range []string{"content", "selection", "resize", "quit"} {
		t.Run(reason, func(t *testing.T) {
			h, pending, details, _ := framePreview(t)
			m := h.m
			pending.Body = "# Pending body"
			details <- prs.DetailUpdate{Detail: pending}
			h.until(t, func() bool { return m.detail.Body == pending.Body })
			late := nextRenderedFrame(t, h)
			switch reason {
			case "content":
				pending.Body = "# Current body"
				details <- prs.DetailUpdate{Detail: pending}
				h.until(t, func() bool { return m.detail.Body == pending.Body })
			case "selection":
				h.deliver(tea.KeyMsg{Type: tea.KeyDown})
			case "resize":
				h.deliver(tea.WindowSizeMsg{Width: 150, Height: 24})
			case "quit":
				h.deliver(tea.KeyMsg{Type: tea.KeyEsc})
			}
			h.deliver(late)
			if strings.Contains(plain(m), "Pending body") {
				t.Fatal("invalidated completed render published")
			}
			switch reason {
			case "content":
				h.until(t, func() bool { return len(m.renderPending) == 0 })
				assertMDColor(t, strings.Join(m.detailLines(), "\n"), "Current body", mdGold)
			case "selection":
				if m.detail != nil || m.detailTop != 0 || strings.Contains(plain(m), "Old styled heading") {
					t.Fatal("selection retained another item's frame")
				}
				h.deliver(tea.KeyMsg{Type: tea.KeyUp})
				h.until(t, func() bool { return m.detail != nil && len(m.renderPending) == 0 })
				assertMDColor(t, strings.Join(m.detailLines(), "\n"), "Pending body", mdGold)
			case "resize":
				if strings.Contains(plainDetail(m), "Old styled heading") || !strings.Contains(m.notices(), "Loading") {
					t.Fatal("resize reused old-width frame or hid render progress")
				}
				h.until(t, func() bool { return len(m.renderPending) == 0 })
				assertMDColor(t, strings.Join(m.detailLines(), "\n"), "Pending body", mdGold)
				assertDimensions(t, m)
			case "quit":
				if !m.quitting || strings.Contains(plainDetail(m), "Pending body") {
					t.Fatal("quit accepted completed rendering")
				}
			}
		})
	}
}
