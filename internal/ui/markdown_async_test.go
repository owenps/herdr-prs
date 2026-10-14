package ui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/owenps/herdr-prs/internal/prs"
)

func TestRenderingCoalescesResizeAndBoundsTemporaryWork(t *testing.T) {
	calls := 0
	m := wideModel(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		calls++
		return prs.Detail{PR: p, Body: strings.Repeat("paragraph\n", 10000)}, nil
	}}, fixture(1))
	_, initial := m.Update(previewRequest(t, m)())
	// Pending paints remain header-only; View/scroll/resize never parse prose.
	for range 5 {
		m.View()
		press(m, tea.KeyTab)
		press(m, tea.KeyDown)
		press(m, tea.KeyTab)
	}
	if strings.Contains(plain(m), "paragraph") || !strings.Contains(m.notices(), "Loading") {
		t.Fatal("pending large prose parsed or flashed before publication")
	}
	// Pending resize commands cancel/coalesce, not fill the CPU/cache.
	cmds := []tea.Cmd{initial}
	for width := 100; width < 150; width++ {
		_, cmd := m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if len(m.renderPending) > 1 {
			t.Fatal("resize work did not coalesce")
		}
	}
	for _, cmd := range cmds[:len(cmds)-1] {
		msg := cmd().(renderedDetailMsg)
		if len(msg.lines) != 0 {
			t.Fatal("cancelled command still rendered")
		}
		m.Update(msg)
	}
	finishRender(t, m, cmds[len(cmds)-1])
	if len(m.renderedDetails) != 1 || calls != 1 || !strings.Contains(plain(m), "paragraph") || strings.Contains(m.notices(), "Loading") {
		t.Fatal("final resize failed to publish prose once with bounded cache")
	}
	for width := 150; width < 152; width++ {
		_, cmd := m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
		finishRender(t, m, cmd)
		if len(m.renderedDetails) != 1 || calls != 1 || !strings.Contains(plain(m), "paragraph") {
			t.Fatal("resize cache unbounded, prose missing, or backend refetched")
		}
		assertDimensions(t, m)
	}
}
