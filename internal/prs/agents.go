package prs

import "path/filepath"

// AgentStatus is Herdr's native, ephemeral attention state, not GitHub status.
// Done means unread completion according to the snapshot's server-side view.
type AgentStatus string

const (
	AgentNone    AgentStatus = ""
	AgentUnknown AgentStatus = "unknown"
	AgentIdle    AgentStatus = "idle"
	AgentWorking AgentStatus = "working"
	AgentBlocked AgentStatus = "blocked"
	AgentDone    AgentStatus = "done"
)

func normalizeAgentStatus(s AgentStatus) AgentStatus {
	switch s {
	case AgentUnknown, AgentIdle, AgentWorking, AgentBlocked, AgentDone:
		return s
	default:
		return AgentUnknown
	}
}

func agentPriority(s AgentStatus) int {
	switch s {
	case AgentBlocked:
		return 5
	case AgentDone:
		return 4
	case AgentWorking:
		return 3
	case AgentIdle:
		return 2
	case AgentUnknown:
		return 1
	default:
		return 0
	}
}

func aggregateAgentStatus(a, b AgentStatus) AgentStatus {
	if agentPriority(b) > agentPriority(a) {
		return b
	}
	return a
}

// AgentStatus aggregates only the preferred navigation target's checkout. A
// current shell can therefore show another agent in that same checkout, never
// an agent in a different checkout of the PR. Agent pane observations take
// precedence over workspace summaries; pathless legacy links are workspace-scoped.
func (p PullRequest) AgentStatus() AgentStatus {
	preferred, ok := p.PreferredLink()
	if !ok {
		return AgentNone
	}
	var panes, workspaces AgentStatus
	var hasAgentPanes bool
	preferredPath := agentCheckoutPath(preferred)
	for _, link := range p.Links {
		if preferredPath != "" {
			if agentCheckoutPath(link) != preferredPath {
				continue
			}
		} else if preferred.WorkspaceID != "" {
			if link.WorkspaceID != preferred.WorkspaceID {
				continue
			}
		} else if link != preferred {
			continue // Missing workspace IDs cannot join unrelated legacy links.
		}
		status := link.AgentStatus
		if link.Agent {
			status = normalizeAgentStatus(status)
		} else if link.PaneID != "" {
			status = AgentNone
		} else if status != AgentNone {
			status = normalizeAgentStatus(status)
		}
		if link.Agent {
			hasAgentPanes = true
			panes = aggregateAgentStatus(panes, status)
		} else if link.PaneID == "" {
			workspaces = aggregateAgentStatus(workspaces, status)
		}
	}
	if hasAgentPanes {
		return panes
	}
	return workspaces
}

// WithAgentSnapshot refreshes only agent presence/status, retaining navigation,
// origin markers, remote fields and clocks. It clones Links and performs no I/O.
// Unknown status alone never establishes presence: modern snapshots enumerate
// recognized agents; older snapshots identify them through Pane.Agent.
func WithAgentSnapshot(p PullRequest, s Snapshot) PullRequest {
	p.Links = append([]Link(nil), p.Links...)
	panes := make(map[string]Pane, len(s.Panes))
	duplicates := make(map[string]bool)
	for _, pane := range s.Panes {
		if pane.ID == "" || pane.WorkspaceID == "" {
			continue
		}
		if _, exists := panes[pane.ID]; exists {
			duplicates[pane.ID] = true
		}
		panes[pane.ID] = pane
	}
	agents := make(map[string]AgentInfo, len(s.Agents))
	for _, agent := range s.Agents {
		pane, ok := panes[agent.PaneID]
		if !ok || duplicates[agent.PaneID] || agent.WorkspaceID != pane.WorkspaceID || !coherentTab(agent.TabID, pane.TabID) {
			continue
		}
		agents[agent.PaneID] = agent
	}
	statuses := make(map[string]AgentStatus, len(panes))
	for id, pane := range panes {
		if duplicates[id] {
			continue
		}
		agent, recognized := agents[id]
		if s.Agents == nil {
			recognized = pane.Agent != ""
		}
		if recognized {
			status := pane.AgentStatus
			if status == AgentNone {
				status = agent.AgentStatus
			}
			statuses[id] = normalizeAgentStatus(status)
		}
	}
	spaces := make(map[string]Workspace, len(s.Workspaces))
	for _, space := range s.Workspaces {
		if space.ID != "" {
			spaces[space.ID] = space
		}
	}
	for i := range p.Links {
		link := &p.Links[i]
		link.Agent, link.AgentStatus = false, AgentNone
		if link.PaneID == "" {
			continue
		}
		pane, ok := panes[link.PaneID]
		if ok && !duplicates[pane.ID] && pane.WorkspaceID == link.WorkspaceID && coherentTab(pane.TabID, link.TabID) && paneMatchesCheckout(pane, *link, spaces[pane.WorkspaceID]) {
			link.AgentStatus = statuses[pane.ID]
			link.Agent = link.AgentStatus != AgentNone
		}
	}
	for i := range p.Links {
		link := &p.Links[i]
		if link.PaneID != "" || link.WorkspaceID == "" || link.Path == "" {
			continue
		}
		if _, exists := spaces[link.WorkspaceID]; !exists {
			continue
		}
		for id, status := range statuses {
			pane := panes[id]
			if pane.WorkspaceID == link.WorkspaceID && paneBelongsToCheckout(pane, *link, p.Links, spaces[pane.WorkspaceID]) {
				link.AgentStatus = aggregateAgentStatus(link.AgentStatus, status)
			}
		}
	}
	return p
}

func coherentTab(a, b string) bool { return a == "" || b == "" || a == b }

// These comparisons are lexical only. Discovery already proved aliases and
// subdirectory roots using Git. An unseen directory is deliberately not assumed
// to belong to an ancestor checkout (it may be a nested repository).
func sameSnapshotPath(a, b string) bool {
	return a != "" && b != "" && filepath.Clean(a) == filepath.Clean(b)
}

func agentCheckoutPath(link Link) string {
	if link.Path != "" {
		return link.Path
	}
	return link.snapshotCheckout
}

func paneMatchesCheckout(pane Pane, link Link, space Workspace) bool {
	root := agentCheckoutPath(link)
	if root == "" {
		return true // Legacy navigation has no checkout identity to contradict.
	}
	if directory := pane.Directory(); directory != "" {
		return sameSnapshotPath(directory, root) || sameSnapshotPath(directory, link.snapshotDirectory)
	}
	if space.Worktree != nil && space.Worktree.CheckoutPath != "" {
		return sameSnapshotPath(space.Worktree.CheckoutPath, root) || sameSnapshotPath(space.Worktree.CheckoutPath, link.snapshotDirectory) || sameSnapshotPath(space.Worktree.CheckoutPath, link.snapshotWorkspacePath)
	}
	return true // Exact pane/workspace identity, no contradictory directory data.
}

func paneBelongsToCheckout(pane Pane, target Link, links []Link, space Workspace) bool {
	if pane.ID == target.snapshotExcludedPane {
		return false
	}
	for _, known := range links {
		if known.PaneID == pane.ID && known.WorkspaceID == pane.WorkspaceID && agentCheckoutPath(known) != "" {
			// A contradictory or stale pane link must not leak through a
			// workspace summary, even if that workspace hosts both checkouts.
			return agentCheckoutPath(known) == target.Path && coherentTab(known.TabID, pane.TabID) && paneMatchesCheckout(pane, known, space)
		}
	}
	directory := pane.Directory()
	if directory == "" && space.Worktree != nil {
		directory = space.Worktree.CheckoutPath
	}
	if sameSnapshotPath(directory, target.Path) || sameSnapshotPath(directory, target.snapshotWorkspacePath) {
		return true
	}
	for _, known := range links {
		if known.WorkspaceID == target.WorkspaceID && agentCheckoutPath(known) == target.Path && sameSnapshotPath(directory, known.snapshotDirectory) {
			return true
		}
	}
	return false
}
