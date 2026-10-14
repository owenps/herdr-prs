package ui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/owenps/herdr-prs/internal/prs"
)

func prAge(p prs.PullRequest, now time.Time) string {
	if p.IsLocal() {
		return ""
	}
	return formatAge(p.CreatedAt, now)
}

func groupAge(p prs.PullRequest, now time.Time) string {
	if p.GroupTiming == nil || p.GroupTiming.Group != p.Group() {
		return ""
	}
	return formatAge(p.GroupTiming.Since, now)
}

func formatAge(since string, now time.Time) string {
	started, err := time.Parse(time.RFC3339Nano, since)
	if err != nil {
		return ""
	}
	seconds := max(int64(0), int64(now.Sub(started)/time.Second))
	switch {
	case seconds < 60:
		return fmt.Sprintf("%02ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		if minutes := seconds / 60 % 60; minutes != 0 {
			return fmt.Sprintf("%dh%dm", seconds/3600, minutes)
		}
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		if hours := seconds / 3600 % 24; hours != 0 {
			return fmt.Sprintf("%dd%dh", seconds/86400, hours)
		}
		return fmt.Sprintf("%dd", seconds/86400)
	}
}

func localLabel(p prs.PullRequest) string {
	if p.Branch != "" {
		return p.Branch
	}
	if p.Local != nil && p.Local.Path != "" {
		return p.Local.Path
	}
	if p.Repository == "" {
		return ""
	}
	return filepath.Base(p.Repository)
}

// Headers are cheap, live metadata. Markdown content has an independent cache.
func detailHeader(p prs.PullRequest, width int, now time.Time) []string {
	lines := wrapDetailText(p.Title, width)
	for i := range lines {
		lines[i] = lipgloss.NewStyle().Bold(true).Render(lines[i])
	}
	prefix := ""
	if age := prAge(p, now); age != "" {
		prefix = "created " + age + " · "
	}
	suffix := ""
	if !p.IsLocal() {
		suffix = fmt.Sprintf(" · %s #%d", p.Repository, p.Number)
	}
	if !p.IsLocal() {
		lines = append(lines, statusLine(prefix, p.Status(), suffix, width, lipgloss.NewStyle(), false))
	}
	if p.IsLocal() {
		path := p.Local.Path
		if path == "" {
			path = localLabel(p)
		}
		if path != "" {
			lines = append(lines, wrapDetailText(path, width)...)
		}
	} else {
		lines = append(lines, wrapDetailText(p.URL, width)...)
	}
	return lines
}

func (m *Model) composeDetail(key detailRenderKey, content []string) []string {
	header := detailHeader(m.detail.PR, key.width, m.clock)
	text := strings.Join(header, "\n")
	if m.frameLines == nil || m.frameKey != key || m.frameHeader != text {
		m.frameKey, m.frameHeader = key, text
		m.frameLines = append(header, content...)
	}
	return m.frameLines
}
