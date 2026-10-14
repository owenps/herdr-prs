package ui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/owenps/herdr-prs/internal/prs"
)

// One plan for the entire filtered list, not just the visible window: columns
// stay put across groups and scrolling. Metadata is bounded; title gets the rest.
type rowLayout struct {
	width, title, iconWidth   int
	age, status, repo, branch int
}

// In review remains available in details/search, not as a redundant list badge.
func rowStatus(p prs.PullRequest) string {
	if p.Status() == "in review" {
		return ""
	}
	return p.Status()
}

func rowReference(p prs.PullRequest) string {
	ref := fmt.Sprintf("#%d", p.Number)
	if len(p.Links) > 0 {
		ref += "↗"
	}
	if current(p) {
		ref += "*"
	}
	return ref
}

func rowRepository(p prs.PullRequest) string {
	if !p.IsLocal() {
		return repositoryName(p.Repository) + " " + rowReference(p)
	}
	name := ""
	if p.Local.Repository != "" {
		name = repositoryName(p.Local.Repository)
	} else if p.Local.Path != "" {
		name = filepath.Base(p.Local.Path)
	}
	if current(p) {
		name += "*"
	}
	return name
}

// Keep the PR number/navigation hint when shortening a long repository name.
func rowRepositoryText(p prs.PullRequest, width int) string {
	if p.IsLocal() {
		return rowFit(rowRepository(p), width)
	}
	ref := rowReference(p)
	if width <= ansi.StringWidth(ref) {
		return rowFit(ref, width)
	}
	return rowFit(repositoryName(p.Repository), width-ansi.StringWidth(ref)-1) + " " + ref
}

func (l rowLayout) metadataWidth() int {
	n := 0
	for _, width := range []int{l.age, l.status, l.repo, l.branch} {
		if width > 0 {
			n += 1 + width // one separator before each column
		}
	}
	return n
}

func listRowLayout(rows []prs.PullRequest, width int, now time.Time) rowLayout {
	l := rowLayout{width: max(0, width)}
	branch := 0
	for _, p := range rows {
		if width >= 24 && p.AgentStatus() != prs.AgentNone {
			l.iconWidth = 2
		}
		l.age = max(l.age, min(10, ansi.StringWidth(groupAge(p, now))))
		l.status = max(l.status, min(14, ansi.StringWidth(singleLine(rowStatus(p)))))
		l.repo = max(l.repo, min(24, ansi.StringWidth(singleLine(rowRepository(p)))))
		branch = max(branch, min(24, ansi.StringWidth(singleLine(p.Branch))))
	}
	// Keep twenty title cells when possible. On tiny viewports title alone wins.
	budget := max(0, width-2-l.iconWidth-min(20, max(0, width-2-l.iconWidth)))
	// Shrink repository first, then status and age; drop only as a last resort.
	for _, field := range []struct {
		width *int
		floor int
	}{{&l.repo, 8}, {&l.status, 7}, {&l.age, 3}} {
		for l.metadataWidth() > budget && *field.width > field.floor {
			*field.width = *field.width - 1
		}
	}
	for _, field := range []*int{&l.repo, &l.status, &l.age} {
		if l.metadataWidth() > budget {
			*field = 0
		}
	}
	// An optional branch must leave a useful title, not consume every saved cell.
	branchBudget := width - min(2, width) - l.iconWidth - l.metadataWidth() - 32 - 1
	if branch > 0 && branchBudget >= min(8, branch) {
		l.branch = min(branch, branchBudget)
	}
	l.title = max(0, width-min(2, width)-l.iconWidth-l.metadataWidth())
	return l
}

// Sanitize before measuring, truncate whole graphemes with ASCII dots only.
func rowFit(text string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(singleLine(text), width, strings.Repeat(".", min(3, width)))
}

func rowCell(text string, width int, right bool) string {
	text = rowFit(text, width)
	padding := strings.Repeat(" ", max(0, width-ansi.StringWidth(text)))
	if right {
		return padding + text
	}
	return text + padding
}

func (l rowLayout) render(p prs.PullRequest, now time.Time, selected bool) string {
	base := lipgloss.NewStyle()
	if selected {
		base = base.Background(lipgloss.AdaptiveColor{Light: "#E5E9F0", Dark: "#434C5E"})
	}
	marker := "  "
	if selected {
		marker = "› "
	}
	out := base.Bold(selected).Render(rowFit(marker, min(2, l.width)))
	if l.iconWidth > 0 {
		icon, color := agentIcon(p.AgentStatus())
		if icon != "" {
			out += base.Foreground(color).Render(icon)
		}
		out += base.Render(strings.Repeat(" ", l.iconWidth-ansi.StringWidth(icon)))
	}
	out += base.Bold(selected).Render(rowCell(p.Title, l.title, false))
	meta := base.Faint(!selected)
	for _, field := range []struct {
		text          string
		width         int
		right, status bool
	}{
		{groupAge(p, now), l.age, true, false},
		{rowStatus(p), l.status, false, true},
		{rowRepositoryText(p, l.repo), l.repo, false, false},
		{p.Branch, l.branch, false, false},
	} {
		if field.width == 0 {
			continue
		}
		out += base.Render(" ")
		if field.status && field.text != "" {
			text := rowFit(field.text, field.width)
			out += base.Foreground(statusColor(p.Status())).Bold(true).Render(text)
			out += base.Render(strings.Repeat(" ", field.width-ansi.StringWidth(text)))
		} else {
			out += meta.Render(rowCell(field.text, field.width, field.right))
		}
	}
	return out
}
