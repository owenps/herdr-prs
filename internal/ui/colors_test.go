package ui

import (
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/cellbuf"
	"github.com/muesli/termenv"
)

func TestStatusPalette(t *testing.T) {
	for status, want := range map[string]string{"feedback": "#D08770", "conflicts": "#BF616A", "checks failing": "#BF616A", "checks running": "#81A1C1", "draft": "#9099AA", "ready": "#A3BE8C", "in review": "#81A1C1", "unknown": "#9099AA", "merged": "#B48EAD", "closed": "#9099AA"} {
		if got := string(statusColor(status)); got != want {
			t.Errorf("%s = %s, want %s", status, got, want)
		}
	}
}

func TestStatusLineColoursOnlyBadgeAndKeepsCellWidth(t *testing.T) {
	renderer := lipgloss.NewRenderer(io.Discard)
	renderer.SetColorProfile(termenv.TrueColor)
	base := renderer.NewStyle().Background(lipgloss.Color("#434C5E"))
	prefix := "↗ · ready/repo #1 · "
	suffix := " · ready-branch"
	prefixWidth, badgeWidth := ansi.StringWidth(prefix), ansi.StringWidth("ready")
	baseStyle := styleAt(t, base.Render("base"), "base")
	badgeStyle := styleAt(t, base.Foreground(lipgloss.Color("#A3BE8C")).Bold(true).Render("ready"), "ready")
	assertStyle := func(out string, want cellbuf.Style) {
		t.Helper()
		for rest, column := ansi.Strip(out), 0; rest != ""; {
			cluster, tail, width, _ := ansi.FirstGraphemeCluster(rest, -1)
			rest = tail
			cell := ansi.Cut(out, column, column+width)
			column += width
			if width == 0 {
				continue
			}
			if got := styleAt(t, cell, cluster); !reflect.DeepEqual(got, want) {
				t.Fatalf("%q: style %+v, want %+v", cluster, got, want)
			}
		}
	}
	for _, padded := range []bool{true, false} {
		for width := 1; width <= 80; width++ {
			got := statusLine(prefix, "ready", suffix, width, base, padded)
			if ansi.StringWidth(got) > width {
				t.Fatalf("width %d: %q", width, got)
			}
			plain := fit(prefix+"ready"+suffix, width)
			if padded {
				plain += strings.Repeat(" ", max(0, width-ansi.StringWidth(plain)))
			}
			if ansi.Strip(got) != plain {
				t.Fatalf("changed content: %q", got)
			}
			if !strings.HasPrefix(plain, prefix+"ready") {
				assertStyle(got, baseStyle)
				continue
			}
			assertStyle(ansi.Cut(got, 0, prefixWidth), baseStyle)
			assertStyle(ansi.Cut(got, prefixWidth, prefixWidth+badgeWidth), badgeStyle)
			assertStyle(ansi.Cut(got, prefixWidth+badgeWidth, width), baseStyle)
		}
	}
	hostile := statusLine("\x1b]52;c;secret\a", "feedback", "\x1b[31munsafe", 80, base, false)
	if strings.Contains(hostile, "secret") || strings.Contains(hostile, "\x1b[31m") {
		t.Fatal("untrusted ANSI survived")
	}
}
