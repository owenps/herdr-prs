package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/owenps/herdr-prs/internal/prs"
)

// Provenance, not TTL freshness, determines whether prose may advance status.
func TestDetailProvenancePreservesCatalogMetadataDuringResize(t *testing.T) {
	for _, tc := range []struct {
		name              string
		cached, fromCache bool
		status            string
	}{
		{"stale disk", true, true, "ready"},
		{"TTL-fresh disk", false, true, "ready"},
		{"live status", false, false, "checks running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := fixture(1)
			current.MergeState, current.UpdatedAt, current.HeadOID = "CLEAN", "current revision", "current head"
			current.Checks = []prs.Check{{Name: "tests", Status: "COMPLETED", Conclusion: "SUCCESS"}}
			current.Links = []prs.Link{{Current: true, WorkspaceID: "current checkout"}}
			current.GroupTiming = &prs.GroupTiming{Group: current.Group(), Since: "2026-01-01T00:00:00Z"}
			old := fixture(1)
			old.URL, old.UpdatedAt, old.HeadOID = "https://obsolete.invalid", "old revision", "old head"
			old.Checks = []prs.Check{{Name: "tests", Status: "IN_PROGRESS"}}
			old.GroupTiming = &prs.GroupTiming{Group: old.Group(), Since: "2025-01-01T00:00:00Z"}
			h := newStreamHarness(t, Actions{
				Load: func(context.Context) (prs.Catalog, error) { return prs.Catalog{PRs: []prs.PullRequest{current}}, nil },
				DetailStream: func(_ context.Context, _ prs.PullRequest, _ bool, emit func(prs.DetailUpdate)) error {
					emit(prs.DetailUpdate{Detail: prs.Detail{PR: old, Body: "disk prose survives resize"}, Cached: tc.cached, FromCache: tc.fromCache})
					return nil
				},
			})
			// Hold the genuine render command while metadata and width are accepted.
			h.m.renderSlot <- struct{}{}
			h.start(h.m.Init())
			h.until(t, func() bool { return h.m.detail != nil && !h.m.detailLoading })
			h.deliver(clockMsg(time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)))
			h.deliver(tea.WindowSizeMsg{Width: 150, Height: 24})
			<-h.m.renderSlot
			h.until(t, func() bool { return len(h.m.renderPending) == 0 })
			view := plain(h.m)
			if !strings.Contains(view, "disk prose survives resize") || !strings.Contains(view, tc.status) || strings.Contains(view, "obsolete.invalid") {
				t.Fatal(view)
			}
			p := h.m.detail.PR
			if p.Status() != tc.status || h.m.catalog.PRs[0].Status() != tc.status || h.m.detailStale != tc.cached || p.URL != current.URL || p.UpdatedAt != current.UpdatedAt || p.HeadOID != current.HeadOID || len(p.Links) != 1 || !p.Links[0].Current || p.Links[0].WorkspaceID != "current checkout" {
				t.Fatalf("prose regressed identity/freshness/checkout metadata: %+v", p)
			}
			wantTiming := current.GroupTiming.Since
			if !tc.cached && !tc.fromCache {
				wantTiming = old.GroupTiming.Since
			}
			if p.GroupTiming == nil || p.GroupTiming.Since != wantTiming || !p.ChecksComplete || !p.FeedbackComplete {
				t.Fatalf("status authority lost group clock/completeness: %+v", p)
			}
		})
	}
}

func TestFailedDetailRefreshKeepsCatalogStatusAndRetainedProse(t *testing.T) {
	for _, catalogFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh catalog", true: "failed catalog"}[catalogFails], func(t *testing.T) {
			old := fixture(1)
			old.MergeState = "CLEAN"
			m := modelWith(t, Actions{Detail: func(context.Context, prs.PullRequest) (prs.Detail, error) {
				return prs.Detail{PR: old, Body: "retained prose"}, nil
			}}, old)
			_, render := m.Update(press(m, tea.KeyCtrlD)())
			finishRender(t, m, render)
			if !strings.Contains(plain(m), "retained prose") || !strings.Contains(plain(m), "ready") {
				t.Fatal("initial detail never painted")
			}
			fresh := old
			fresh.Unresolved = 1
			m.actions.Load = func(context.Context) (prs.Catalog, error) {
				if catalogFails {
					return prs.Catalog{}, errors.New("catalog offline")
				}
				return prs.Catalog{PRs: []prs.PullRequest{fresh}}, nil
			}
			m.actions.Detail = func(context.Context, prs.PullRequest) (prs.Detail, error) {
				return prs.Detail{}, errors.New("detail offline")
			}
			_, request := m.Update(press(m, tea.KeyCtrlR)())
			if request == nil {
				t.Fatal("refresh did not retry active detail")
			}
			m.Update(request())
			wantStatus, wantUnresolved := "feedback", 1
			if catalogFails {
				wantStatus, wantUnresolved = "ready", 0
			}
			view := plain(m)
			if !m.detailStale || m.stale != catalogFails || m.detail.PR.Status() != wantStatus || m.catalog.PRs[0].Unresolved != wantUnresolved || !strings.Contains(view, wantStatus) || !strings.Contains(view, "detail offline") || !strings.Contains(view, "retained prose") || strings.Contains(view, "STALE") || strings.Contains(view, "Loading") {
				t.Fatalf("detail failure hid status/prose or contaminated catalog freshness: %s", view)
			}
			press(m, tea.KeyEsc)
			if !strings.Contains(plain(m), wantStatus) || strings.Contains(plain(m), "Loading") || m.rows[0].Unresolved != wantUnresolved {
				t.Fatal("return to list lost authoritative status")
			}
		})
	}
}
