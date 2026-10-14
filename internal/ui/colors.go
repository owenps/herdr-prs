package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Nord shades: status remains foreground-coloured even on selected rows.
func statusColor(status string) lipgloss.Color {
	switch status {
	case "feedback":
		return "#D08770"
	case "conflicts", "checks failing":
		return "#BF616A"
	case "checks running", "in review":
		return "#81A1C1"
	case "ready":
		return "#A3BE8C"
	case "merged":
		return "#B48EAD"
	default:
		return "#9099AA" // draft, unknown, closed
	}
}

// Sanitize and truncate before styling. Never interpret PR-provided ANSI as UI
// colours, or colour a status-like word in a repository, title or branch.
func statusLine(prefix, status, suffix string, width int, base lipgloss.Style, padded bool) string {
	prefix, status, suffix = singleLine(prefix), singleLine(status), singleLine(suffix)
	plain := fit(prefix+status+suffix, width)
	if padded {
		plain += strings.Repeat(" ", max(0, width-ansi.StringWidth(plain)))
	}
	if !strings.HasPrefix(plain, prefix+status) {
		return base.Render(plain)
	}
	tail := strings.TrimPrefix(plain, prefix+status)
	badge := base.Faint(false).Foreground(statusColor(status)).Bold(true)
	return base.Render(prefix) + badge.Render(status) + base.Render(tail)
}
