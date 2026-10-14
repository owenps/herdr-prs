package ui

import (
	"context"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/owenps/herdr-prs/internal/prs"
)

// Herdr v0.9.3 src/client/shell.rs status_icon/status_color, native Dots
// mode with the Nord palette. Done means server-side unread, not PR merged.
func agentIcon(status prs.AgentStatus) (string, lipgloss.Color) {
	switch status {
	case prs.AgentBlocked:
		return "●", "#BF616A"
	case prs.AgentWorking:
		return "●", "#EBCB8B"
	case prs.AgentDone:
		return "●", "#8FBCBB"
	case prs.AgentIdle:
		return "○", "#A3BE8C"
	case prs.AgentUnknown:
		return "·", "#4C566A"
	default:
		return "", ""
	}
}

const agentPollInterval = time.Second

type agentSnapshotMsg struct {
	token    uint64
	snapshot prs.Snapshot
	err      error
}

// Reserve the entire timer/request cycle. Only its accepted result schedules
// another cycle, so slow calls, repeated Init and clock ticks cannot overlap.
func (m *Model) pollAgents() tea.Cmd {
	if m.actions.AgentSnapshot == nil || m.agentPolling || m.quitting || m.ctx.Err() != nil {
		return nil
	}
	m.agentPolling = true
	m.agentToken++
	token, ctx, fn := m.agentToken, m.ctx, m.actions.AgentSnapshot
	return func() tea.Msg {
		timer := time.NewTimer(agentPollInterval)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return nil
		}
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		snapshot, err := fn(requestCtx)
		return agentSnapshotMsg{token, cloneAgentSnapshot(snapshot), err}
	}
}

func cloneAgentSnapshot(snapshot prs.Snapshot) prs.Snapshot {
	snapshot.Panes = slices.Clone(snapshot.Panes)
	snapshot.Agents = slices.Clone(snapshot.Agents)
	snapshot.Tabs = slices.Clone(snapshot.Tabs)
	snapshot.Workspaces = slices.Clone(snapshot.Workspaces)
	for i := range snapshot.Workspaces {
		if worktree := snapshot.Workspaces[i].Worktree; worktree != nil {
			copy := *worktree
			snapshot.Workspaces[i].Worktree = &copy
		}
	}
	return snapshot
}

// Apply the newest model-owned overlay at every catalog ingress, including
// cached/late refresh results. Exact checkout matching lives in prs, not here.
func (m *Model) withAgentMetadata(p prs.PullRequest) prs.PullRequest {
	if m.agentSnapshot != nil {
		p = prs.WithAgentSnapshot(p, *m.agentSnapshot)
	}
	if m.agentErr != "" {
		p.Links = slices.Clone(p.Links)
		for i := range p.Links {
			link := &p.Links[i]
			if link.Agent || link.AgentStatus != prs.AgentNone {
				link.AgentStatus = prs.AgentUnknown
			}
		}
	}
	return p
}

func (m *Model) acceptAgentSnapshot(msg agentSnapshotMsg) tea.Cmd {
	if !m.agentPolling || msg.token != m.agentToken || m.quitting || m.ctx.Err() != nil {
		return nil
	}
	m.agentPolling = false
	if msg.err != nil {
		m.agentErr = "Herdr status unavailable"
	} else {
		snapshot := cloneAgentSnapshot(msg.snapshot)
		m.agentSnapshot = &snapshot
		m.agentErr = ""
	}
	selected, _ := m.selected()
	for i, p := range m.catalog.PRs {
		m.catalog.PRs[i] = m.withAgentMetadata(p)
	}
	// Presence is searchable, including previously filtered-out rows. Status
	// never affects group/priority; retain selection and viewport where possible.
	m.rank(selected.Key(), false)
	// Unchanged identity/revision reuses prose; a presence-filtered selection
	// change follows the normal preview cache/debounce path. Fullscreen stays pinned.
	return tea.Batch(m.pollAgents(), m.syncActiveDetail())
}
