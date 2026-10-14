package ui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/cellbuf"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

// Compare rendered colors directly: the ANSI parser's RGBA conversion can
// round a channel down by one, so converting it back to hex isn't lossless.
func assertRenderedForeground(t *testing.T, out, target, color string) cellbuf.Style {
	t.Helper()
	pen := styleAt(t, out, target)
	reference := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render("reference")
	if !reflect.DeepEqual(pen.Fg, styleAt(t, reference, "reference").Fg) {
		t.Fatalf("%q foreground differs from %s", target, color)
	}
	return pen
}

func TestGroupHeadingTintNeutralRuleAndCellBudgets(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	for _, tc := range []struct {
		group prs.Group
		color string
	}{
		{prs.GroupDone, "#b48ead"},
		{prs.GroupInReview, "#81a1c1"},
		{prs.GroupInProgress, "#ebcb8b"},
		{prs.GroupCancelled, "#9099aa"},
	} {
		t.Run(string(tc.group), func(t *testing.T) {
			out := groupHeading(listGroup{name: tc.group, count: 3}, 40)
			for _, token := range []string{string(tc.group), "(3)"} {
				pen := assertRenderedForeground(t, out, token, tc.color)
				if pen.Attrs&cellbuf.FaintAttr == 0 {
					t.Fatal("group label/count lost faint styling")
				}
			}
			pen := styleAt(t, out, "─")
			if pen.Fg != nil || pen.Attrs&cellbuf.FaintAttr == 0 {
				t.Fatalf("heading color leaked into neutral rule: %+v", pen)
			}
		})
	}
	// Geometry varies with width, not with a group's palette.
	for width := 0; width <= 200; width++ {
		out := groupHeading(listGroup{name: prs.GroupInProgress, count: 3}, width)
		plain := ansi.Strip(out)
		if ansi.StringWidth(out) != width || strings.ContainsAny(plain, "\n\r") || plain != expectedGroupHeading("In Progress (3)", width) {
			t.Fatalf("width %d: wrapped/incorrect heading: %q", width, out)
		}
		if width == 0 && out != "" {
			t.Fatal("zero width emitted escape sequences")
		}
	}
}
