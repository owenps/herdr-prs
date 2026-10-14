package ui

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

type detailRenderKey struct {
	identity string
	content  [32]byte
	width    int
	profile  termenv.Profile
}

type renderedDetailMsg struct {
	epoch uint64
	key   detailRenderKey
	lines []string
}

func (m *Model) renderKey(width int) detailRenderKey {
	// Accepted/cached details are immutable and replaced, never edited in
	// place. Hash once per replacement, not on every View/scroll/clamp.
	if m.renderSource != m.detail {
		content := *m.detail
		// Catalog/header changes (including age) do not reparse Markdown.
		p := content.PR
		content.PR = prs.PullRequest{Checks: p.Checks, ChecksComplete: p.ChecksComplete, FeedbackComplete: p.FeedbackComplete}
		if p.IsLocal() {
			content.PR.Local = &prs.LocalWork{}
		}
		data, _ := json.Marshal(content)
		m.renderSource, m.renderContent = m.detail, sha256.Sum256(data)
	}
	return detailRenderKey{m.detail.PR.Key(), m.renderContent, max(1, width), m.markdownProfile}
}

func (m *Model) clearRenderedDetails() {
	m.renderEpoch++
	clear(m.renderedDetails)
	for _, cancel := range m.renderPending {
		cancel()
	}
	clear(m.renderPending)
	m.fallbackLines = nil
}

func (m *Model) renderDetail() tea.Cmd {
	if m.detail == nil {
		return nil
	}
	key := m.renderKey(m.detailWidth())
	// One width per identity bounds resize cache growth. Coalesce superseded
	// queued work, and allow at most one renderer per popup to consume CPU.
	for old := range m.renderedDetails {
		if old.identity == key.identity && old != key {
			delete(m.renderedDetails, old)
		}
	}
	for old, cancel := range m.renderPending {
		if old != key {
			cancel()
			delete(m.renderPending, old)
		}
	}
	if _, ok := m.renderedDetails[key]; ok || m.renderPending[key] != nil {
		return nil
	}
	if m.frontLines != nil && m.frontKey == key {
		// Ctrl-R may clear semantic caches, but an identical live response
		// can reuse the complete front buffer without parsing again.
		m.renderedDetails[key] = m.frontLines
		return nil
	}
	m.renderEpoch++
	ctx, cancel := context.WithCancel(m.ctx)
	m.renderPending[key] = cancel
	// clampDetail may have composed a temporary fallback before this command
	// was queued. Recompose first paint as header-only while rendering.
	m.frameLines = nil
	// Clone slices, share immutable strings. No model data is touched by
	// workers, including during cancellation/late delivery.
	detail := cloneRenderDetail(*m.detail)
	epoch, slot := m.renderEpoch, m.renderSlot
	return func() tea.Msg {
		defer cancel()
		select {
		case <-ctx.Done():
			return renderedDetailMsg{epoch: epoch, key: key}
		case slot <- struct{}{}:
		}
		defer func() { <-slot }()
		if ctx.Err() != nil {
			return renderedDetailMsg{epoch: epoch, key: key}
		}
		lines := formatDetailContent(detail, key.width, func(body string) []string {
			if ctx.Err() != nil {
				return nil
			}
			return renderMarkdown(body, key.width, key.profile)
		})
		return renderedDetailMsg{epoch, key, lines}
	}
}

func cloneRenderDetail(d prs.Detail) prs.Detail {
	if d.PR.GroupTiming != nil {
		timing := *d.PR.GroupTiming
		d.PR.GroupTiming = &timing
	}
	if d.PR.Local != nil {
		local := *d.PR.Local
		d.PR.Local = &local
	}
	d.PR.Checks = slices.Clone(d.PR.Checks)
	d.PR.Links = slices.Clone(d.PR.Links)
	d.Reviews = slices.Clone(d.Reviews)
	d.Comments = slices.Clone(d.Comments)
	d.Threads = slices.Clone(d.Threads)
	for i := range d.Threads {
		d.Threads[i].Comments = slices.Clone(d.Threads[i].Comments)
	}
	d.Warnings = slices.Clone(d.Warnings)
	return d
}

// GitHub records arrive chronologically, including persisted older details.
// Reverse a clone first: missing/invalid legacy timestamps and equal timestamps
// retain deterministic newest-input-first ordering without touching cache data.
func newestFirst[T any](records []T, timestamp func(T) string) []T {
	ordered := slices.Clone(records)
	slices.Reverse(ordered)
	slices.SortStableFunc(ordered, func(a, b T) int {
		at, _ := time.Parse(time.RFC3339Nano, timestamp(a))
		bt, _ := time.Parse(time.RFC3339Nano, timestamp(b))
		return bt.Compare(at)
	})
	return ordered
}

// Keep the latest submission, including COMMENTED, for each known login.
// Deleted/unknown authors are independent reviews, not one anonymous person.
func latestReviews(reviews []prs.Review) []prs.Review {
	seen := make(map[string]bool)
	var latest []prs.Review
	for _, review := range newestFirst(reviews, func(r prs.Review) string { return r.SubmittedAt }) {
		login := strings.ToLower(strings.TrimSpace(review.Author))
		if login != "" {
			if seen[login] {
				continue
			}
			seen[login] = true
		}
		latest = append(latest, review)
	}
	return latest
}

func wrapDetailText(s string, width int) []string {
	lines := strings.Split(ansi.Hardwrap(sanitize(s), max(1, width), true), "\n")
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, max(1, width), "…")
	}
	return lines
}

// Only prose goes to Markdown. API labels/paths/headings remain literal text;
// a safe plain-text fallback keeps content visible while the
// expensive rendering command runs off the Bubble Tea event loop.
func formatDetail(d prs.Detail, width int, markdown func(string) []string) []string {
	return append(detailHeader(d.PR, width, time.Time{}), formatDetailContent(d, width, markdown)...)
}

func formatDetailContent(d prs.Detail, width int, markdown func(string) []string) []string {
	p := d.PR
	if p.IsLocal() {
		lines := wrapDetailText("No pull request linked yet.", width)
		muted := lipgloss.NewStyle().Foreground(lipgloss.Color(mdMuted))
		for i := range lines {
			lines[i] = muted.Render(lines[i])
		}
		return lines
	}
	var lines []string
	metadata := func(ss ...string) {
		for _, s := range ss {
			lines = append(lines, wrapDetailText(s, width)...)
		}
	}
	body := func(s, empty string) {
		if s == "" {
			metadata(empty)
			return
		}
		lines = append(lines, markdown(s)...)
	}
	section := func(label string) {
		divider := ansi.Truncate("─ "+label+" "+strings.Repeat("─", max(1, width)), max(1, width), "")
		lines = append(lines, "", lipgloss.NewStyle().Faint(true).Render(divider))
	}
	section("Body")
	body(d.Body, "(empty)")
	section("Checks")
	if len(p.Checks) == 0 {
		metadata("(none)")
	}
	for _, c := range p.Checks {
		state := c.Conclusion
		if state == "" {
			state = c.State
		}
		if state == "" {
			state = c.Status
		}
		metadata(c.Name + " · " + state)
	}
	if !p.ChecksComplete {
		metadata("Checks incomplete")
	}
	section("Reviews")
	if len(d.Reviews) == 0 {
		metadata("(none)")
	}
	for _, r := range latestReviews(d.Reviews) {
		metadata(r.Author + " · " + r.State)
		body(r.Body, "(no comment)")
	}
	section("Unresolved threads")
	unresolved := 0
	for _, t := range d.Threads {
		if t.Resolved {
			continue
		}
		unresolved++
		metadata(fmt.Sprintf("%s:%d", t.Path, t.Line))
		for _, c := range newestFirst(t.Comments, func(c prs.Comment) string { return c.CreatedAt }) {
			metadata(c.Author + ":")
			body(c.Body, "(no comment)")
		}
	}
	if unresolved == 0 {
		metadata("(none)")
	}
	if !p.FeedbackComplete {
		metadata("Feedback incomplete")
	}
	section("Conversation")
	if len(d.Comments) == 0 {
		metadata("(none)")
	}
	for i, c := range newestFirst(d.Comments, func(c prs.Comment) string { return c.CreatedAt }) {
		if i > 0 {
			divider := lipgloss.NewStyle().Foreground(lipgloss.Color(mdMuted)).Faint(true).Render(strings.Repeat("─", max(1, width)))
			lines = append(lines, "", divider)
		}
		metadata(c.Author + " · " + c.CreatedAt)
		body(c.Body, "(no comment)")
	}
	if warnings := displayWarnings(d.Warnings); len(warnings) > 0 {
		section("Warnings")
		metadata(warnings...)
	}
	return lines
}
