package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/owenps/herdr-prs/internal/prs"
)

func cloneCatalog(c prs.Catalog) prs.Catalog {
	out := prs.Catalog{PRs: make([]prs.PullRequest, len(c.PRs)), Warnings: append([]string(nil), c.Warnings...)}
	for i, p := range c.PRs {
		out.PRs[i] = cloneRenderDetail(prs.Detail{PR: p}).PR
	}
	return out
}

// Old disk entries can contain backend-injected freshness notices. Suppress
// only those redundant messages; authentication/network/truncation warnings stay.
func displayWarnings(warnings []string) []string {
	var visible []string
	for _, warning := range warnings {
		switch strings.ToLower(strings.TrimSpace(warning)) {
		case "cached github data; refresh pending or unavailable",
			"github detail refresh unavailable; showing cached data":
			continue
		}
		visible = append(visible, warning)
	}
	return visible
}

func repositoryName(repository string) string {
	_, name, ok := strings.Cut(singleLine(repository), "/")
	if ok {
		return name
	}
	return singleLine(repository)
}

func (m *Model) applyCatalog(update prs.CatalogUpdate) tea.Cmd {
	selected, _ := m.selected()
	m.catalog = cloneCatalog(update.Catalog)
	for i, p := range m.catalog.PRs {
		m.catalog.PRs[i] = m.withAgentMetadata(p)
	}
	m.loaded, m.stale = true, update.Cached
	if !update.Cached {
		m.err = ""
	}
	preserve := selected.Key()
	if replacement, ok := linkedReplacement(selected, m.catalog.PRs); ok {
		preserve = replacement.Key()
		if m.detailKey == selected.Key() && preserve != selected.Key() {
			m.clearDetail()
			if m.inDetail {
				m.detailKey = preserve
			}
		}
	}
	m.rank(preserve, !m.initialSelectionDone)
	if len(m.rows) > 0 {
		m.initialSelectionDone = true
	}
	return m.syncActiveDetail()
}

// Exact identity wins. Across PR↔local transitions, follow the selected
// checkout, never another checkout sharing its workspace. Ambiguity stays put.
func linkedReplacement(selected prs.PullRequest, rows []prs.PullRequest) (prs.PullRequest, bool) {
	for _, p := range rows {
		if p.Key() == selected.Key() {
			return p, true
		}
	}
	link, ok := selected.PreferredLink()
	if !ok {
		return prs.PullRequest{}, false
	}
	path := link.Path
	if path == "" && selected.Local != nil {
		path = selected.Local.Path
	}
	var match prs.PullRequest
	found := false
	for _, p := range rows {
		if p.IsLocal() == selected.IsLocal() {
			continue
		}
		for _, candidate := range p.Links {
			matches := path != "" && candidate.Path == path
			if path == "" {
				matches = link.WorkspaceID != "" && candidate.WorkspaceID == link.WorkspaceID
			}
			if matches {
				if found && match.Key() != p.Key() {
					return prs.PullRequest{}, false
				}
				match, found = p, true
				break
			}
		}
	}
	return match, found
}

func (m *Model) syncActiveDetail() tea.Cmd {
	// Keep catalog metadata authoritative even when prose/version is unchanged.
	if p, ok := m.detailPR(); ok && m.detail != nil {
		d := *m.detail
		d.PR = p
		m.detail = &d
	}
	if !m.inDetail {
		return m.syncPreview(false)
	}
	p, ok := m.detailPR()
	if !ok {
		key := m.detailKey
		m.clearDetail()
		m.detailKey = key
		m.detailErr = "Item no longer in catalog; Esc to return"
		return nil
	}
	if m.detailKey == p.Key() && m.detailVersion == version(p) {
		return m.renderDetail()
	}
	return m.fetchDetail(p)
}

// Copy live status and its group clock: identity, freshness and checkout metadata must not
// regress when a detail backend returns an older/differently shaped PR record.
func withDetailStatus(p, detail prs.PullRequest) prs.PullRequest {
	p.State, p.Draft = detail.State, detail.Draft
	p.GroupTiming = detail.GroupTiming
	p.ReviewDecision, p.MergeState, p.Mergeable = detail.ReviewDecision, detail.MergeState, detail.Mergeable
	p.Checks, p.Unresolved = detail.Checks, detail.Unresolved
	p.ChecksComplete, p.FeedbackComplete = detail.ChecksComplete, detail.FeedbackComplete
	return p
}

func (m *Model) applyDetail(key string, update prs.DetailUpdate) tea.Cmd {
	p, ok := m.detailPR()
	if !ok {
		return nil
	}
	d := cloneRenderDetail(update.Detail)
	if !update.Cached && !update.FromCache {
		oldGroup := p.Group()
		selected, _ := m.selected()
		p = withDetailStatus(p, d.PR)
		// Same-group status changes must not move rows under navigation.
		for i := range m.catalog.PRs {
			if m.catalog.PRs[i].Key() == key {
				m.catalog.PRs[i] = p
			}
		}
		for i := range m.rows {
			if m.rows[i].Key() == key {
				m.rows[i] = p
			}
		}
		if p.Group() != oldGroup {
			// A lifecycle/draft transition must keep sections contiguous, while
			// following the selected identity without restarting its detail work.
			m.rank(selected.Key(), false)
		}
		m.detailErr = ""
	}
	// Cached prose is useful; cached status is not authoritative over catalog.
	d.PR = p
	m.detail, m.detailStale = &d, update.Cached
	if !update.Cached {
		m.detailCache[key] = d
		m.detailVersions[key] = m.detailVersion
	}
	// Searching group names can filter out a live transition. Keep its cache,
	// but hide the old preview immediately; fullscreen stays pinned to this PR.
	if m.split() {
		selected, ok := m.selected()
		if !ok || selected.Key() != key {
			return m.syncPreview(false)
		}
	}
	m.clampDetail()
	return m.renderDetail()
}
