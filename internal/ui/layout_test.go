package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

func TestGlobalChromeAndTwoThirdsLayout(t *testing.T) {
	p := fixture(1)
	m := modelWith(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		return prs.Detail{PR: p, Body: "# Markdown heading\n\n**bold** prose"}, nil
	}}, p)
	for _, width := range []int{80, 99, 100, 101, 120, 200} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
			if m.split() && m.detail == nil {
				_, render := m.Update(previewRequest(t, m)())
				finishRender(t, m, render)
			}
			assertDimensions(t, m)
			view := plain(m)
			lines := strings.Split(view, "\n")
			for _, text := range []string{"jump to worktree", "1/1 items", "Enter jump/details", "List ·"} {
				if strings.Count(view, text) != 1 {
					t.Fatalf("global %q not exactly once: %s", text, view)
				}
			}
			divider := strings.Repeat("─", width)
			if lines[1] != divider || lines[len(lines)-2] != divider || strings.Count(view, divider) != 2 {
				t.Fatalf("missing global dividers: %s", view)
			}
			if strings.Contains(view, "Preview") || strings.Contains(view, "Pull request details") {
				t.Fatal("redundant detail heading")
			}
			if !m.split() {
				if strings.Contains(view, "│") {
					t.Fatal("narrow list has columns")
				}
				return
			}
			left, right := m.paneWidths()
			if left != (width-1)*2/3 || left+right+1 != width || width == 100 && right != 33 {
				t.Fatalf("pane ratio: %d / %d at %d", left, right, width)
			}
			for i, row := range lines[3 : len(lines)-2] {
				parts := strings.SplitN(row, "│", 2)
				if len(parts) != 2 || ansi.StringWidth(parts[0]) != left || ansi.StringWidth(parts[1]) > right {
					t.Fatalf("misaligned body row %d: %q", i, row)
				}
				if i == 0 && parts[1] != p.Title {
					t.Fatalf("right does not start with title: %q", parts[1])
				}
				if i == 1 && !strings.Contains(parts[1], "in review · owner/repo #1") {
					t.Fatalf("missing detail metadata: %q", parts[1])
				}
				if i == 2 && !strings.HasPrefix(parts[1], "https://github.com/") {
					t.Fatalf("missing detail URL: %q", parts[1])
				}
			}
			if len(m.paneBody(left, false)) != m.contentHeight() || len(m.paneBody(right, true)) != m.contentHeight() {
				t.Fatal("panes have different viewport heights")
			}
		})
	}
}

func TestDetailHeadersWrapLiteralUnicodeMetadataWithinCellBudget(t *testing.T) {
	p := fixture(1)
	p.Title = "**literal 界👩‍💻e\u0301 title** " + strings.Repeat("界👩‍💻e\u0301", 20) + "\x1b]52;c;PAYLOAD\a\u202e"
	p.Repository = "owner/*literal-repo*"
	p.CreatedAt = "2026-01-01T00:00:00Z"
	local := localFixture("local", "/work/界👩‍💻e\u0301-checkout")
	local.Title, local.CreatedAt = p.Title, p.CreatedAt
	now := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
	for width := 1; width <= 200; width++ {
		for _, row := range []prs.PullRequest{p, local} {
			out := strings.Join(detailHeader(row, width, now), "\n")
			for _, line := range strings.Split(out, "\n") {
				if ansi.StringWidth(line) > width || strings.ContainsAny(ansi.Strip(line), "\r\u202e") || strings.Contains(out, "PAYLOAD") {
					t.Fatalf("width %d: unsafe/overflowing literal header: %q", width, out)
				}
			}
			if width == 200 && !strings.Contains(ansi.Strip(out), "**literal 界👩‍💻e\u0301 title**") {
				t.Fatal("header parsed Markdown or lost Unicode text: " + out)
			}
		}
	}
}

func TestSearchCursorInsertionAndUnicodeTail(t *testing.T) {
	m := modelWith(t, Actions{}, fixture(1))
	m.markdownProfile = termenv.Ascii
	for _, tc := range []struct {
		width int
		query string
		want  string
	}{
		{1, "abc", "|"},
		{2, "abc", "❯|"},
		{3, "abc", "❯ |"},
		{4, "界e\u0301", "❯ e\u0301|"},
		{5, "ae\u0301界", "❯ 界|"},
		{6, "👩‍💻e\u0301", "❯ 👩‍💻e\u0301|"},
		{5, "xe\u0301👩‍💻", "❯ 👩‍💻|"},
		{4, "🇺🇸e\u0301", "❯ e\u0301|"},
		{4, "e\u0301界", "❯ |"},
		{6, "abcd\x1b[31m界\x1b[0m\x1b]52;c;payload\a", "❯ d界|"},
	} {
		m.width, m.query = tc.width, tc.query
		got := m.searchHeader()
		if got != tc.want || ansi.StringWidth(got) > tc.width {
			t.Errorf("width %d query %q: %q, want %q", tc.width, tc.query, got, tc.want)
		}
	}
	m.width, m.query = 80, ""
	if got := m.searchHeader(); !strings.HasPrefix(got, "❯ | jump to worktree") || !strings.HasSuffix(got, "All") || ansi.StringWidth(got) != m.width {
		t.Fatalf("placeholder moved insertion cell: %q", got)
	}
	m.width, m.query = 120, strings.Repeat("界👩‍💻e\u0301", 80)+"END"
	header := m.searchHeader()
	left, _ := m.paneWidths()
	if !strings.HasSuffix(strings.TrimRight(strings.TrimSuffix(header, "All"), " "), "END|") || strings.Index(header, "…") >= 0 || ansi.StringWidth(header) != 120 || ansi.StringWidth(header) <= left+1 {
		t.Fatalf("query does not span columns/keep end cursor: %q", header)
	}
	if !strings.HasPrefix(header, "❯ 界") && !strings.HasPrefix(header, "❯ 👩‍💻") && !strings.HasPrefix(header, "❯ e\u0301") {
		t.Fatalf("tail split a grapheme: %q", header)
	}
}

func TestSearchCursorFollowsFocusAndControls(t *testing.T) {
	m := wideModel(t, Actions{}, fixture(1), fixture(2))
	m.markdownProfile = termenv.TrueColor
	typeQuery(m, "Change")
	header := m.searchHeader()
	if !strings.HasPrefix(ansi.Strip(header), "❯ Change ") || !strings.Contains(header, "\x1b[97;7m \x1b[0m") {
		t.Fatalf("missing insertion cell: %q", header)
	}
	press(m, tea.KeyTab)
	if m.searchHeader() != header || !strings.Contains(plain(m), "Details ·") {
		t.Fatal("preview hid cursor or lost footer focus")
	}
	press(m, tea.KeySpace)
	typeQuery(m, "2")
	if m.query != "Change 2" || chosen(m) != 2 || m.previewFocused {
		t.Fatal("preview typing did not return to list/search")
	}
	press(m, tea.KeyTab)
	press(m, tea.KeyBackspace)
	if m.query != "Change " || m.previewFocused || m.quitting {
		t.Fatal("preview backspace did not edit search")
	}
	typeQuery(m, "2")
	header = m.searchHeader()
	press(m, tea.KeyCtrlD)
	if !m.inDetail || m.searchHeader() != header || !strings.Contains(plain(m), "Details ·") {
		t.Fatal("fullscreen hid cursor or lost actual focus")
	}
	press(m, tea.KeyEsc)
	if m.inDetail || m.searchHeader() != header || m.query != "Change 2" {
		t.Fatal("return from fullscreen lost search")
	}
}

func TestTinyHeightBudgetAndStateRetention(t *testing.T) {
	m := modelWith(t, Actions{}, fixture(1), fixture(2))
	for _, width := range []int{1, 2, 3, 9, 40, 99, 100, 120} {
		for height := 1; height <= 12; height++ {
			for _, full := range []bool{false, true} {
				m.inDetail = full
				m.Update(tea.WindowSizeMsg{Width: width, Height: height})
				assertDimensions(t, m)
				if m.contentHeight() < 1 {
					t.Fatal("chrome consumed entire body")
				}
			}
		}
	}
	for _, full := range []bool{false, true} {
		m.inDetail = full
		for height := 1; height <= 8; height++ {
			m.Update(tea.WindowSizeMsg{Width: 120, Height: height})
			m.stale, m.detailStale, m.err, m.detailErr = true, true, "Refresh: offline", "Details: denied"
			m.catalog.Warnings = []string{"partial catalog"}
			assertDimensions(t, m)
			view := plain(m)
			if strings.Contains(view, "STALE") {
				t.Fatalf("height %d fullscreen %v exposed internal freshness: %s", height, full, view)
			}
			for _, want := range []string{"offline", "denied", "partial catalog"} {
				if !strings.Contains(view, want) {
					t.Fatalf("height %d fullscreen %v lost %q: %s", height, full, want, view)
				}
			}
		}
	}
}
