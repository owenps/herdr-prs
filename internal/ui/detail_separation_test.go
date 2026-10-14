package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/cellbuf"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

func TestLocalDetailsUseNeutralHeaderAndMutedUnlinkedCaption(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	p := localFixture("local", "/work/checkout")
	p.CreatedAt = "2025-01-01T00:00:00Z"
	p.GroupTiming = &prs.GroupTiming{Group: p.Group(), Since: p.CreatedAt, Approximate: true}
	for _, width := range []int{1, 2, 16, 80} {
		lines := formatDetailContent(prs.Detail{PR: p}, width, func(string) []string { t.Fatal("local caption parsed as Markdown"); return nil })
		for _, line := range lines {
			plain := ansi.Strip(line)
			if ansi.StringWidth(line) > width {
				t.Fatal("caption overflow", width, line)
			}
			if plain != "" {
				assertRenderedForeground(t, line, plain, mdMuted)
			}
		}
	}
	m := modelWith(t, Actions{}, p)
	_, render := m.Update(tea.WindowSizeMsg{Width: 120, Height: 12})
	if render == nil {
		t.Fatal("local preview did not schedule rendering")
	}
	finishRender(t, m, render)
	out := strings.Join(m.detailLines(), "\n")
	lines := strings.Split(ansi.Strip(out), "\n")
	if len(lines) != 3 || lines[0] != "Local work" || lines[1] != "/work/checkout" || lines[2] != "No pull request linked yet." {
		t.Fatalf("local details invented creation/status metadata or a header gap: %q", out)
	}
	if styleAt(t, out, p.Title).Fg != nil || styleAt(t, out, p.Local.Path).Fg != nil {
		t.Fatal("caption color leaked into title/path")
	}
	assertRenderedForeground(t, out, "No pull request linked yet.", mdMuted)
}

func TestConversationMessagesHaveMutedDividersOnlyBetweenMessages(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	for count := 0; count <= 3; count++ {
		for _, width := range []int{30, 80} {
			t.Run(fmt.Sprintf("%d messages/%d cells", count, width), func(t *testing.T) {
				d := prs.Detail{PR: fixture(1)}
				for i := 0; i < count; i++ {
					d.Comments = append(d.Comments, prs.Comment{Author: fmt.Sprintf("Author%d", i), Body: fmt.Sprintf("Message%d", i), CreatedAt: fmt.Sprintf("2026-01-0%dT00:00:00Z", i+1)})
				}
				lines := formatDetailContent(d, width, func(body string) []string { return wrapDetailText(body, width) })
				rule := strings.Repeat("─", width)
				var separators []int
				for i, line := range lines {
					if ansi.StringWidth(line) > width {
						t.Fatal("conversation overflow", line)
					}
					if ansi.Strip(line) == rule {
						separators = append(separators, i)
						pen := assertRenderedForeground(t, line, rule, mdMuted)
						if pen.Attrs&cellbuf.FaintAttr == 0 {
							t.Fatal("conversation divider not faint")
						}
						if i == 0 || lines[i-1] != "" || i+1 == len(lines) || !strings.HasPrefix(ansi.Strip(lines[i+1]), "Author") {
							t.Fatal("divider not between messages", lines)
						}
					}
				}
				if len(separators) != max(0, count-1) {
					t.Fatal("missing or orphan message divider", separators)
				}
				for i := 0; i < count; i++ {
					if styleAt(t, strings.Join(lines, "\n"), fmt.Sprintf("Author%d", i)).Fg != nil {
						t.Fatal("divider color leaked into message author")
					}
				}
				if count > 0 && !strings.HasPrefix(ansi.Strip(lines[len(lines)-1]), "Message") {
					t.Fatal("trailing message separator", lines)
				}
			})
		}
	}
}
