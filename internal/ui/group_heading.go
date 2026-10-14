package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/owenps/herdr-prs/internal/prs"
)

func groupColor(group prs.Group) lipgloss.Color {
	switch group {
	case prs.GroupDone:
		return statusColor("merged")
	case prs.GroupInReview:
		return statusColor("checks running")
	case prs.GroupInProgress:
		return "#EBCB8B"
	default:
		return statusColor("closed")
	}
}

func groupHeading(group listGroup, width int) string {
	if width <= 0 {
		return ""
	}
	label := fit(fmt.Sprintf("%s (%d)", group.name, group.count), width)
	out := lipgloss.NewStyle().Foreground(groupColor(group.name)).Faint(true).Render(label)
	if remaining := width - ansi.StringWidth(label); remaining > 0 {
		out += lipgloss.NewStyle().Faint(true).Render(" " + strings.Repeat("─", remaining-1))
	}
	return out
}
