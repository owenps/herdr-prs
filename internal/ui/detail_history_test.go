package ui

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

func TestPersistedLegacyWarningsFilteredOnlyForDisplay(t *testing.T) {
	legacy := []string{"Cached GitHub data; refresh pending or unavailable", "GitHub detail refresh unavailable; showing cached data"}
	real := []string{"Authentication unavailable; run gh auth login", "Network request failed; retry", "Reviews truncated at 100"}
	warnings := append(append([]string{}, legacy...), real...)
	m := modelWith(t, Actions{Load: func(context.Context) (prs.Catalog, error) {
		return prs.Catalog{PRs: []prs.PullRequest{fixture(1)}, Warnings: warnings}, nil
	}})
	d := prs.Detail{PR: fixture(1), Warnings: warnings}
	// Exercise the shape retained in older disk caches without rewriting it.
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var persisted prs.Detail
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	before := cloneRenderDetail(persisted)
	for _, width := range []int{1, 3, 33, 120} {
		lines := formatDetail(persisted, width, func(s string) []string { return wrapDetailText(s, width) })
		for _, line := range lines {
			if ansi.StringWidth(line) > width {
				t.Fatalf("warning overflow: %q", line)
			}
		}
		if width != 120 {
			continue
		}
		out := ansi.Strip(strings.Join(lines, "\n")) + "\n" + m.notices()
		for _, warning := range legacy {
			if strings.Contains(out, warning) {
				t.Fatalf("legacy notice displayed: %s", out)
			}
		}
		for _, warning := range real {
			if strings.Count(out, warning) != 2 {
				t.Fatalf("real warning lost: %s", out)
			}
		}
	}
	if !reflect.DeepEqual(before, persisted) || !reflect.DeepEqual(warnings, m.catalog.Warnings) {
		t.Fatal("display filtering mutated cache warnings")
	}
	filtered := strings.Join(formatDetail(prs.Detail{PR: fixture(1), Warnings: legacy}, 80, func(s string) []string { return []string{s} }), "\n")
	if strings.Contains(filtered, "Warnings") {
		t.Fatal("empty legacy warning section")
	}
}

func TestCachedDetailNewestFirstWithoutMutatingInput(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		reviewTimes, commentTimes []string
		order                     []string
	}{
		{"timestamps", []string{"2025-01-01T00:00:00Z", "2025-01-03T01:00:00+01:00", "2025-01-02T00:00:00Z"}, []string{"2025-01-01T00:00:00Z", "2025-01-03T00:00:00Z", "2025-01-02T00:00:00Z"}, []string{"B", "C", "A"}},
		{"old reviews", []string{"", "", ""}, []string{"2025-01-01T00:00:00Z", "2025-01-02T00:00:00Z", "2025-01-03T00:00:00Z"}, []string{"C", "B", "A"}},
		{"all missing", []string{"", "", ""}, []string{"", "", ""}, []string{"C", "B", "A"}},
		{"equal timestamps", []string{"2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z", "2025-01-01T00:00:00Z"}, []string{"", "", ""}, []string{"C", "B", "A"}},
		{"mixed legacy", []string{"", "2025-01-01T00:00:00Z", ""}, []string{"", "2025-01-01T00:00:00Z", ""}, []string{"B", "C", "A"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := prs.Detail{PR: fixture(1), Threads: []prs.Thread{{Path: "file.go", Line: 1, Comments: []prs.Comment{{Body: "thread-old"}, {Body: "thread-new"}}}}}
			for i, id := range []string{"A", "B", "C"} {
				d.Reviews = append(d.Reviews, prs.Review{Body: "review-" + id, SubmittedAt: tc.reviewTimes[i]})
				d.Comments = append(d.Comments, prs.Comment{Body: "comment-" + id, CreatedAt: tc.commentTimes[i]})
			}
			data, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			var cached prs.Detail
			if err := json.Unmarshal(data, &cached); err != nil {
				t.Fatal(err)
			}
			before := cloneRenderDetail(cached)
			updates := make(chan prs.DetailUpdate, 2)
			updates <- prs.DetailUpdate{Detail: cached, Cached: true}
			h := newStreamHarness(t, Actions{
				Load: func(context.Context) (prs.Catalog, error) { return prs.Catalog{PRs: []prs.PullRequest{d.PR}}, nil },
				DetailStream: func(ctx context.Context, _ prs.PullRequest, _ bool, emit func(prs.DetailUpdate)) error {
					for {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case update := <-updates:
							emit(update)
						}
					}
				},
			})
			m := h.m
			m.markdownProfile = termenv.Ascii
			h.start(m.Init())
			h.until(t, func() bool { return m.detail != nil && len(m.renderPending) == 0 })
			h.deliver(tea.KeyMsg{Type: tea.KeyCtrlD})
			h.until(t, func() bool { return len(m.renderPending) == 0 })
			checkOrder := func(lines []string) {
				t.Helper()
				out := ansi.Strip(strings.Join(lines, "\n"))
				for _, prefix := range []string{"review-", "comment-"} {
					previous := -1
					for _, id := range tc.order {
						index := strings.Index(out, prefix+id)
						if index <= previous {
							t.Fatalf("%s not newest-first: %s", prefix, out)
						}
						previous = index
					}
				}
				if strings.Index(out, "thread-new") >= strings.Index(out, "thread-old") {
					t.Fatal("inline conversation not newest-first")
				}
			}
			checkOrder(m.detailLines()) // Markdown and rendered cache reuse.
			checkOrder(m.detailLines()) // Missing timestamps must not toggle ordering.
			if !reflect.DeepEqual(before, cached) || !reflect.DeepEqual(before.Reviews, m.detail.Reviews) || !reflect.DeepEqual(before.Comments, m.detail.Comments) || !reflect.DeepEqual(before.Threads, m.detail.Threads) {
				t.Fatal("render sorting mutated detail/cache input")
			}
			changed := cloneRenderDetail(cached)
			changed.Reviews[0].SubmittedAt = "2026-01-01T00:00:00Z"
			updates <- prs.DetailUpdate{Detail: changed}
			h.until(t, func() bool {
				return m.detail.Reviews[0].SubmittedAt == "2026-01-01T00:00:00Z" && len(m.renderPending) == 0
			})
			out := ansi.Strip(strings.Join(m.detailLines(), "\n"))
			if strings.Index(out, "review-A") > strings.Index(out, "review-B") || !strings.Contains(out, "review-A") {
				t.Fatal("new review timestamp did not reorder rendered prose: " + out)
			}
		})
	}
}
