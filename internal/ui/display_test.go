package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/cellbuf"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

func TestSearchStylesAreIndependentIncludingCursorWhitespace(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	m := modelWith(t, Actions{}, fixture(1))
	m.markdownProfile = termenv.TrueColor
	header := m.searchHeader()
	prompt := styleAt(t, header, "❯")
	cursor := styleAt(t, header, "  jump") // first space is the insertion cell
	placeholder := styleAt(t, header, "jump to worktree")
	if cursor.Fg != ansi.BrightWhite || cursor.Attrs&cellbuf.ReverseAttr == 0 || cursor.Attrs&cellbuf.FaintAttr != 0 {
		t.Fatalf("cursor whitespace not independently white: %#v", cursor)
	}
	if prompt.Fg != ansi.Cyan || prompt.Attrs&cellbuf.FaintAttr != 0 || placeholder.Attrs&cellbuf.FaintAttr == 0 || placeholder.Fg == prompt.Fg {
		t.Fatalf("placeholder/prompt styles coupled: %q", header)
	}
	m.query = "Typed query"
	header = m.searchHeader()
	typed := styleAt(t, header, m.query)
	if typed.Fg != prompt.Fg || typed.Attrs&cellbuf.FaintAttr != 0 || strings.Contains(header, "jump to worktree") {
		t.Fatalf("typed input inherited placeholder style: %q", header)
	}
	for _, focus := range []string{"preview", "full"} {
		m.previewFocused, m.inDetail = focus == "preview", focus == "full"
		if got := m.searchHeader(); got != header {
			t.Fatal("detail focus changed independently styled search cursor/input")
		}
		m.query = ""
		got := m.searchHeader()
		cursor := styleAt(t, got, "  jump")
		placeholder := styleAt(t, got, "jump to worktree")
		if cursor.Fg != ansi.BrightWhite || cursor.Attrs&cellbuf.ReverseAttr == 0 || cursor.Attrs&cellbuf.FaintAttr != 0 || placeholder.Attrs&cellbuf.FaintAttr == 0 {
			t.Fatal("detail focus coupled cursor/placeholder styles")
		}
		m.query = "Typed query"
	}
	for width := 1; width <= 30; width++ {
		m.width = width
		for _, query := range []string{"", "界👩‍💻e\u0301 typed query"} {
			m.query = query
			if ansi.StringWidth(m.searchHeader()) > width {
				t.Fatal("styled header overflow")
			}
		}
	}
}

func TestDetailSectionDividersDimLabelsWithoutChangingMarkdown(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	d := prs.Detail{PR: fixture(1), Body: "# Gold prose\n\n`violet prose`", Warnings: []string{"literal warning"}}
	sections := []string{"Body", "Checks", "Reviews", "Unresolved threads", "Conversation", "Warnings"}
	for _, width := range []int{1, 2, 5, 19, 33, 80} {
		lines := formatDetail(d, width, func(body string) []string { return renderMarkdown(body, width, termenv.TrueColor) })
		for _, line := range lines {
			if ansi.StringWidth(line) > width {
				t.Fatalf("section overflow at %d: %q", width, line)
			}
		}
		if width < 33 {
			continue
		}
		out := strings.Join(lines, "\n")
		for _, label := range sections {
			if !strings.Contains(ansi.Strip(out), "─ "+label+" ─") || styleAt(t, out, label).Attrs&cellbuf.FaintAttr == 0 {
				t.Fatalf("missing dim section divider %q: %q", label, out)
			}
		}
		assertMDColor(t, out, "Gold prose", mdGold)
		assertMDColor(t, out, "violet prose", mdViolet)
		if styleAt(t, out, "literal warning").Attrs&cellbuf.FaintAttr != 0 {
			t.Fatal("section faint leaked into content")
		}
	}
}

func TestRepositoryOnlyInRowsFullIdentityStillSearchable(t *testing.T) {
	p := fixture(1)
	p.Repository = "specific-organization/visible-repo"
	m := modelWith(t, Actions{}, p)
	for _, width := range []int{80, 120} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
		rows := ansi.Strip(strings.Join(m.paneBody(width, false), "\n"))
		if !strings.Contains(rows, "visible-repo #1") || strings.Contains(rows, "specific-organization") {
			t.Fatal(rows)
		}
	}
	typeQuery(m, "specific-organization")
	if len(m.rows) != 1 || m.rows[0].Key() != p.Key() || m.rows[0].Repository != p.Repository {
		t.Fatal("visual shortening altered identity/search")
	}
	detail := strings.Join(formatDetail(prs.Detail{PR: p}, 80, func(s string) []string { return []string{s} }), "\n")
	if !strings.Contains(detail, p.Repository+" #1") {
		t.Fatal("full details lost organization")
	}
}
