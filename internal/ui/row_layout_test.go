package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

func TestListColumnsStayAlignedAcrossGroupsAndViewport(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	done, review := fixture(1), fixture(2)
	done.Title, done.State, done.Repository = "Completed title", "MERGED", "org/done-repo"
	review.Title, review.Repository = "Review title", "other/review-repo"
	done.GroupTiming = &prs.GroupTiming{Group: done.Group(), Since: now.Add(-3 * time.Second).Format(time.RFC3339)}
	review.GroupTiming = &prs.GroupTiming{Group: review.Group(), Since: now.Add(-49 * time.Hour).Format(time.RFC3339)}
	local := localFixture("local", "/work/path-fallback")
	local.Title, local.Local.Repository = "Local title", "real-owner/checkout-repo"
	local.CreatedAt = done.GroupTiming.Since // never used as a list clock
	local.GroupTiming = &prs.GroupTiming{Group: local.Group(), Since: now.Add(-123 * time.Minute).Format(time.RFC3339), Approximate: true}
	cancelled := fixture(3)
	cancelled.Title, cancelled.State = "Cancelled title", "CLOSED"
	m := modelWith(t, Actions{}, done, review, local, cancelled)
	m.Update(clockMsg(now))
	m.Update(tea.WindowSizeMsg{Width: 99, Height: 30})
	view := plain(m)
	var ageEnd, statusStart, repoStart int
	var localLine string
	for _, want := range []struct{ title, age, status, repo, branch string }{
		{"Completed title", "03s", "merged", "done-repo #1", "topic-1"},
		{"Review title", "2d1h", "", "review-repo #2", "topic-2"},
		{"Local title", "2h3m", "", "checkout-repo*", "topic"},
		{"Cancelled title", "", "closed", "repo #3", "topic-3"},
	} {
		var row string
		for _, line := range strings.Split(view, "\n") {
			if strings.Contains(line, want.title) {
				row = line
			}
		}
		title := strings.Index(row, want.title)
		if title < 0 || ansi.StringWidth(row[:title]) != 2 || !strings.Contains(row, want.repo) || !strings.Contains(row, want.branch) || ansi.StringWidth(row) != 99 {
			t.Fatalf("one-line title/identity/branch lost for %q: %q", want.title, row)
		}
		repo := strings.Index(row, want.repo)
		repoColumn := ansi.StringWidth(row[:repo])
		if repoStart != 0 && repoColumn != repoStart {
			t.Fatalf("repository column shifted across groups: %q", row)
		}
		repoStart = repoColumn
		ageByteEnd := 0
		if want.age != "" {
			age := strings.Index(row, want.age)
			if age < 0 {
				t.Fatalf("group age missing: %q", row)
			}
			ageByteEnd = age + len(want.age)
			column := ansi.StringWidth(row[:ageByteEnd])
			if ageEnd != 0 && column != ageEnd {
				t.Fatalf("group ages not right-aligned: %q", row)
			}
			ageEnd = column
		}
		if want.status != "" {
			status := strings.Index(row, want.status)
			if status < 0 {
				t.Fatalf("status missing: %q", row)
			}
			column := ansi.StringWidth(row[:status])
			if statusStart != 0 && column != statusStart {
				t.Fatalf("status column shifted across groups: %q", row)
			}
			statusStart = column
		} else if strings.TrimSpace(row[ageByteEnd:repo]) != "" {
			t.Fatalf("blank status cell invented a local/review badge: %q", row)
		}
		if want.title == "Local title" {
			localLine = row
		}
	}
	// A two-line viewport contains only the selected local and its heading.
	// Its columns still reflect the entire filtered list, not that visible row.
	m.Update(tea.WindowSizeMsg{Width: 99, Height: 7})
	if !strings.Contains(plain(m), localLine) {
		t.Fatalf("viewport changed selected row's columns: %q", plain(m))
	}
}

// One width sweep owns row geometry, Unicode safety and metadata priority.
// Palette/selection styling belongs to the row badge contract tests.
func TestListRowsPreserveTitleAndAlignedMetadataWithinCellBudget(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	p := fixture(1)
	p.Title = "界👩‍💻e\u0301 neutral title " + strings.Repeat("界👩‍💻e\u0301", 40) + "\x1b[31mred\x1b[0m\x1b]52;c;SECRET\a\u202e\nEND"
	p.Repository = "org/" + strings.Repeat("界👩‍💻repository", 10) + "\x1b[31m"
	p.Branch = "branch-界👩‍💻\x1b]52;c;SECRET\a\n"
	p.GroupTiming = &prs.GroupTiming{Group: p.Group(), Since: "2026-01-01T00:00:00Z"}
	p.Checks = []prs.Check{{Status: "IN_PROGRESS"}}
	p.Links = []prs.Link{{Current: true}}
	local := localFixture("checkout", "/work/界👩‍💻checkout")
	local.Title, local.Local.Repository = "Local neutral title", "owner/界👩‍💻checkout"
	local.Branch = "local-branch"
	now := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
	local.GroupTiming = &prs.GroupTiming{Group: local.Group(), Since: now.Add(-10000 * 24 * time.Hour).Format(time.RFC3339), Approximate: true}
	unknown := fixture(2)
	unknown.CreatedAt, unknown.UpdatedAt = p.GroupTiming.Since, p.GroupTiming.Since
	rows := []prs.PullRequest{p, local, unknown}
	for width := 1; width <= 200; width++ {
		columns := listRowLayout(rows, width, now)
		if width >= 22 && columns.title < 20 || columns.branch > 0 && columns.title < 32 {
			t.Fatalf("width %d sacrificed useful title to metadata: %+v", width, columns)
		}
		var ageEnd int
		for _, row := range rows {
			for _, selected := range []bool{false, true} {
				out := columns.render(row, now, selected)
				plain := ansi.Strip(out)
				if ansi.StringWidth(out) != width || strings.ContainsAny(plain, "\n\r\u202e") || strings.Contains(out, "SECRET") || strings.Contains(out, "\x1b[31m") || strings.Contains(plain, "…") {
					t.Fatalf("width %d row %s: unsafe/wrapped/overflow: %q", width, row.Key(), out)
				}
				if width >= 74 && row.Number == 1 && (!strings.Contains(plain, "#1↗*") || !strings.Contains(plain, "checks running") || !strings.Contains(plain, "...")) {
					t.Fatal("truncation lost reference, navigation hint or status: " + plain)
				}
				if width >= 100 && row.GroupTiming != nil {
					want := "3d"
					if row.IsLocal() {
						want = "10000d"
					}
					index := strings.Index(plain, want)
					if index < 0 || strings.Contains(plain, "~") {
						t.Fatalf("age missing/truncated or approximation marker displayed: %q", plain)
					}
					end := ansi.StringWidth(plain[:index]) + len(want)
					if ageEnd != 0 && end != ageEnd {
						t.Fatalf("mixed rows shifted age metadata: %d != %d: %q", end, ageEnd, plain)
					}
					ageEnd = end
				}
				if width == 200 {
					title, branch := "界👩‍💻e\u0301 neutral title", "branch-界👩‍💻"
					if row.IsLocal() {
						title, branch = "Local neutral title", "local-branch"
					} else if row.Number == 2 {
						title, branch = "Change 2", "topic-2"
					}
					if !strings.Contains(plain, title) || !strings.Contains(plain, branch) {
						t.Fatal("wide row lost literal title/Unicode branch: " + plain)
					}
				}
			}
		}
	}
	// Whole emoji and combining clusters survive truncation; ASCII dots use
	// their actual three-cell budget rather than the old single-cell ellipsis.
	if got := rowFit("👩‍💻e\u0301abcdef", 6); got != "👩‍💻e\u0301..." {
		t.Fatalf("split grapheme or wrong truncation budget: %q", got)
	}
}
