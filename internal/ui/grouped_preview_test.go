package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/owenps/herdr-prs/internal/prs"
)

func filteredMigrationModel(t *testing.T, survivor, scope string) (*Model, prs.PullRequest, prs.PullRequest, *int) {
	t.Helper()
	p := fixture(1)
	p.Draft = true
	p.Links = []prs.Link{{Current: true}}
	ps := []prs.PullRequest{p}
	var next prs.PullRequest
	switch survivor {
	case "draft":
		next = fixture(2)
		next.Draft = true
	case "local":
		next = localFixture("local", "/work/local")
		next.Links = nil
	}
	if survivor != "empty" {
		ps = append(ps, next)
	}
	calls := 0
	m := wideModel(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		calls++
		body := "NEW PREVIEW"
		if p.Number == 1 {
			body = "OLD PROSE"
		}
		return prs.Detail{PR: p, Body: body}, nil
	}}, ps...)
	if scope == "query" {
		typeQuery(m, "In Progress")
	} else {
		for range 3 {
			press(m, tea.KeyShiftTab)
		}
	}
	// Select through list navigation, not rank/private cursor assignment.
	press(m, tea.KeyHome)
	for i := 0; i < len(m.rows) && chosen(m) != p.Number; i++ {
		press(m, tea.KeyDown)
	}
	_, render := m.Update(previewRequest(t, m)())
	finishRender(t, m, render)
	if selected, _ := m.selected(); selected.Key() != p.Key() || !strings.Contains(plain(m), "OLD PROSE") {
		t.Fatal("test did not select/render draft")
	}
	return m, p, next, &calls
}

func TestFilteredLiveGroupMigrationSyncsPreviewAndRejectsLateReplies(t *testing.T) {
	for _, scope := range []string{"query", "group"} {
		for _, survivor := range []string{"draft", "local", "empty"} {
			t.Run(scope+"/"+survivor, func(t *testing.T) {
				m, p, next, calls := filteredMigrationModel(t, survivor, scope)
				query, group := m.query, m.groupFilter
				oldToken, oldVersion := m.detailToken, m.detailVersion
				old := *m.detail
				old.Body = "LATE OLD PROSE"
				_, render := m.Update(detailUpdateMsg{token: oldToken, key: p.Key(), update: prs.DetailUpdate{Detail: old}})
				// Complete rendering but hold its delivery until after selection changes.
				lateRender := render().(renderedDetailMsg)
				fresh := old
				fresh.PR.Draft, fresh.PR.Unresolved = false, 3
				fresh.Body = "MIGRATED PROSE"
				_, follow := m.Update(detailUpdateMsg{token: oldToken, key: p.Key(), update: prs.DetailUpdate{Detail: fresh}})
				if m.query != query || m.groupFilter != group || m.detailToken == oldToken || strings.Contains(plain(m), "OLD PROSE") || strings.Contains(plain(m), "MIGRATED PROSE") {
					t.Fatal("transition retained old preview/generation")
				}
				cached, ok := m.detailCache[p.Key()]
				if !ok || cached.Body != fresh.Body || cached.PR.Draft || cached.PR.Unresolved != 3 || m.detailVersions[p.Key()] != oldVersion {
					t.Fatal("transition lost old PR cache/live status")
				}
				for _, catalog := range m.catalog.PRs {
					if catalog.Key() == p.Key() && (catalog.Group() != prs.GroupInReview || catalog.Unresolved != 3) {
						t.Fatal("catalog lost live group/status")
					}
				}
				if survivor == "empty" {
					if _, ok := m.selected(); ok || m.detail != nil || m.detailKey != "" || m.detailLoading || m.previewFocused || follow != nil {
						t.Fatal("empty selection did not clear preview")
					}
				} else {
					selected, _ := m.selected()
					if selected.Key() != next.Key() || m.detailKey != next.Key() || follow == nil || !strings.Contains(plain(m), "In Progress (1)") {
						t.Fatal("survivor did not become active preview")
					}
					if survivor == "local" {
						finishRender(t, m, follow)
						if m.detail == nil || m.detail.PR.Key() != next.Key() || m.detailLoading || !strings.Contains(plain(m), "No pull request linked yet.") {
							t.Fatal("local survivor did not render immediately")
						}
					} else if m.detail != nil || !m.detailLoading || !m.detailDebouncing {
						t.Fatal("draft survivor did not hide old frame/start debounce")
					}
				}
				newToken, loading := m.detailToken, m.detailLoading
				for _, late := range []tea.Msg{
					lateRender,
					detailUpdateMsg{token: oldToken, key: p.Key(), update: prs.DetailUpdate{Detail: old}},
					detailMsg{token: oldToken, key: p.Key(), detail: old},
					detailMsg{token: oldToken, key: p.Key(), completeOnly: true},
					detailMsg{token: oldToken, key: p.Key(), completeOnly: true, err: errors.New("late failure")},
				} {
					m.Update(streamMsg{message: late})
					if _, ok := m.renderedDetails[lateRender.key]; ok {
						t.Fatal("late old render entered render cache")
					}
					if m.detailToken != newToken || m.detailLoading != loading || m.detailErr != "" || m.detailCache[p.Key()].Body != fresh.Body || strings.Contains(plain(m), "OLD PROSE") {
						t.Fatal("late old reply interrupted replacement preview/cache")
					}
				}
				if survivor == "draft" {
					_, render = m.Update(previewRequest(t, m)())
					finishRender(t, m, render)
					if m.detail.PR.Key() != next.Key() || !strings.Contains(plain(m), "NEW PREVIEW") || m.detailLoading {
						t.Fatal("replacement detail did not render")
					}
				}
				wantCalls := 1
				if survivor == "draft" {
					wantCalls++
				}
				if *calls != wantCalls {
					t.Fatalf("unexpected fetch count: got %d want %d", *calls, wantCalls)
				}
			})
		}
	}
}

func TestFilteredLiveGroupMigrationFullscreenStaysPinnedUntilReturn(t *testing.T) {
	for _, scope := range []string{"query", "group"} {
		for _, survivor := range []string{"draft", "local", "empty"} {
			t.Run(scope+"/"+survivor, func(t *testing.T) {
				m, p, next, calls := filteredMigrationModel(t, survivor, scope)
				finishRender(t, m, press(m, tea.KeyCtrlD))
				fresh := *m.detail
				fresh.PR.Draft = false
				fresh.Body = strings.Repeat("PINNED PROSE\n\n", 30)
				token := m.detailToken
				_, render := m.Update(detailUpdateMsg{token: token, key: p.Key(), update: prs.DetailUpdate{Detail: fresh}})
				finishRender(t, m, render)
				press(m, tea.KeyPgDown)
				scroll := m.detailTop
				if !m.inDetail || m.detail.PR.Key() != p.Key() || m.detailKey != p.Key() || m.detailToken != token || scroll == 0 || !strings.Contains(plain(m), "PINNED PROSE") {
					t.Fatal("fullscreen migration did not stay pinned")
				}
				if selected, ok := m.selected(); ok && selected.Key() == p.Key() {
					t.Fatal("transition should filter fullscreen PR out of list")
				}
				m.Update(detailMsg{token: token, key: p.Key(), completeOnly: true})
				if m.detail.PR.Key() != p.Key() || m.detailTop != scroll || *calls != 1 {
					t.Fatal("fullscreen stream completion changed pin/scroll")
				}
				follow := press(m, tea.KeyEsc)
				if m.inDetail || m.detailTop != 0 || strings.Contains(plain(m), "PINNED PROSE") {
					t.Fatal("return retained pinned preview")
				}
				switch survivor {
				case "draft":
					if m.detailKey != next.Key() || !m.detailLoading || follow == nil {
						t.Fatal("return did not schedule survivor")
					}
				case "local":
					finishRender(t, m, follow)
					if m.detail.PR.Key() != next.Key() || *calls != 1 {
						t.Fatal("return did not locally render survivor")
					}
				case "empty":
					if m.detail != nil || m.detailKey != "" || follow != nil {
						t.Fatal("return to empty list did not clear preview")
					}
				}
			})
		}
	}
}
