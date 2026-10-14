// Package ui presents a keyboard-only PR popup. All state changes happen in Update;
// commands only return messages, and never mutate the model.
package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/owenps/herdr-prs/internal/prs"
)

type Actions struct {
	Load   func(context.Context) (prs.Catalog, error)
	Detail func(context.Context, prs.PullRequest) (prs.Detail, error)
	// Optional incremental backends; Load/Detail remain the fallback.
	LoadStream   func(context.Context, bool, func(prs.CatalogUpdate)) error
	DetailStream func(context.Context, prs.PullRequest, bool, func(prs.DetailUpdate)) error
	Jump         func(context.Context, prs.Link) error
	OpenURL      func(context.Context, string) error
	// AgentSnapshot reads Herdr only; it never refreshes Git/GitHub data.
	AgentSnapshot func(context.Context) (prs.Snapshot, error)
}

// Run owns request lifetimes, including when the terminal or parent context exits.
func Run(ctx context.Context, actions Actions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m := NewModel(ctx, actions)
	defer m.cancel()
	// Intentional quits cancel requests, not the program's parent context.
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}

type loadMsg struct {
	token        uint64
	catalog      prs.Catalog
	err          error
	completeOnly bool
}
type catalogUpdateMsg struct {
	token  uint64
	update prs.CatalogUpdate
}
type detailUpdateMsg struct {
	token  uint64
	key    string
	update prs.DetailUpdate
}
type detailMsg struct {
	token        uint64
	key          string
	detail       prs.Detail
	err          error
	completeOnly bool
}

type prVersion struct {
	key, updatedAt, headOID string
}

func version(p prs.PullRequest) prVersion {
	return prVersion{p.Key(), p.UpdatedAt, p.HeadOID}
}

// A timer carries the same generation as its eventual request. Superseded
// timers never call the API, even when cancellation and delivery race.
type previewMsg struct {
	token uint64
	key   string
	force bool
}

// A single popup-owned clock updates ages, never backend data.
type clockMsg time.Time

type actionMsg struct {
	token uint64
	jump  bool
	err   error
}

// Model is also usable without a terminal: call Init, deliver command results to
// Update, and render View. Commands must not be executed on the UI thread.
type Model struct {
	ctx                                    context.Context
	cancel                                 context.CancelFunc
	actions                                Actions
	width, height                          int
	clock                                  time.Time
	frontKey                               detailRenderKey
	frontLines                             []string
	frameKey                               detailRenderKey
	frameHeader                            string
	frameLines                             []string
	catalog                                prs.Catalog
	rows                                   []prs.PullRequest
	query                                  string
	groupFilter                            prs.Group // Empty means all groups; independent of the search query.
	cursor, top                            int
	loaded, loading, stale                 bool
	err                                    string
	inDetail, detailLoading, detailStale   bool
	detailDebouncing                       bool
	detailKey                              string
	detail                                 *prs.Detail
	detailTop                              int
	previewFocused                         bool
	detailErr                              string
	detailCache                            map[string]prs.Detail
	detailVersions                         map[string]prVersion
	detailVersion                          prVersion
	forceDetail, initialSelectionDone      bool
	renderedDetails                        map[detailRenderKey][]string
	renderPending                          map[detailRenderKey]context.CancelFunc
	renderSlot                             chan struct{}
	renderSource                           *prs.Detail
	renderContent                          [32]byte
	fallbackKey                            detailRenderKey
	fallbackLines                          []string
	renderEpoch                            uint64
	markdownProfile                        termenv.Profile
	loadToken, detailToken, actionToken    uint64
	loadCancel, detailCancel, actionCancel context.CancelFunc
	actionPending, quitting                bool
	initialized                            bool
	agentToken                             uint64
	agentPolling                           bool
	agentSnapshot                          *prs.Snapshot
	agentErr                               string
}

var _ tea.Model = (*Model)(nil)

func NewModel(ctx context.Context, actions Actions) *Model {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Model{ctx: ctx, cancel: cancel, actions: actions, width: 80, height: 24, clock: time.Now(),
		detailCache: make(map[string]prs.Detail), detailVersions: make(map[string]prVersion), renderedDetails: make(map[detailRenderKey][]string),
		renderPending: make(map[detailRenderKey]context.CancelFunc), renderSlot: make(chan struct{}, 1), markdownProfile: markdownProfile()}
}

func (m *Model) Init() tea.Cmd {
	if m.initialized || m.quitting || m.ctx.Err() != nil {
		return nil
	}
	m.initialized = true
	return tea.Batch(m.load(false), m.tick(), m.pollAgents())
}

func (m *Model) tick() tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case now := <-timer.C:
			return clockMsg(now)
		}
	}
}

func (m *Model) load(force bool) tea.Cmd {
	if m.loadCancel != nil {
		m.loadCancel()
	}
	m.loadToken++
	token := m.loadToken
	ctx, cancel := context.WithCancel(m.ctx)
	m.loadCancel = cancel
	m.loading = true
	if force {
		// Explicit refresh bypasses both backend TTLs, not just popup caches.
		clear(m.detailCache)
		clear(m.detailVersions)
		m.clearRenderedDetails()
		m.invalidateDetail()
		m.detailVersion = prVersion{}
		m.forceDetail = true
		m.detailStale = m.detail != nil
	}
	if fn := m.actions.LoadStream; fn != nil {
		cmd := streamCommand(ctx, cancel, func(emit func(tea.Msg)) error {
			return fn(ctx, force, func(update prs.CatalogUpdate) {
				update.Catalog = cloneCatalog(update.Catalog)
				emit(catalogUpdateMsg{token, update})
			})
		}, func(err error) tea.Msg { return loadMsg{token: token, err: err, completeOnly: true} })
		if force {
			return tea.Batch(cmd, m.syncActiveDetail())
		}
		return cmd
	}
	fn := m.actions.Load
	return func() tea.Msg {
		defer cancel()
		if err := ctx.Err(); err != nil {
			return loadMsg{token: token, err: err}
		}
		if fn == nil {
			return loadMsg{token: token, err: fmt.Errorf("loading unavailable")}
		}
		catalog, err := fn(ctx)
		return loadMsg{token: token, catalog: catalog, err: err}
	}
}

func (m *Model) invalidateDetail() {
	if m.detailCancel != nil {
		m.detailCancel()
	}
	m.detailToken++
	m.detailLoading, m.detailDebouncing = false, false
}

const previewDelay = 120 * time.Millisecond

func (m *Model) split() bool { return m.width >= 100 && !m.inDetail }

func (m *Model) paneWidths() (int, int) {
	// At 100 columns: 66 list cells, one separator, 33 detail cells.
	left := (m.width - 1) * 2 / 3
	return left, m.width - left - 1
}

func (m *Model) detailWidth() int {
	if m.split() {
		_, right := m.paneWidths()
		return right
	}
	return m.width
}

func (m *Model) clearDetail() {
	m.invalidateDetail()
	m.detail, m.detailKey, m.detailErr = nil, "", ""
	m.detailStale, m.detailTop = false, 0
	m.frontLines, m.frameLines = nil, nil
	for key, cancel := range m.renderPending {
		cancel()
		delete(m.renderPending, key)
	}
	m.renderEpoch++
	m.detailVersion = prVersion{}
	m.forceDetail = false
}

// Selection changes hide old content immediately, before the debounce expires.
// Same-key refresh may retain content while the request is pending.
func (m *Model) syncPreview(force bool) tea.Cmd {
	if !m.split() {
		return nil
	}
	p, ok := m.selected()
	if !ok {
		m.clearDetail()
		m.previewFocused = false
		return nil
	}
	if !force && m.detailKey == p.Key() && m.detailVersion == version(p) {
		return m.renderDetail()
	}
	force = force || m.forceDetail
	if m.detailKey != p.Key() {
		m.clearDetail()
	}
	m.forceDetail = force
	m.invalidateDetail()
	m.detailKey, m.detailErr = p.Key(), ""
	m.detailVersion = version(p)
	if p.IsLocal() {
		return m.fetchDetail(p)
	}
	if cached, ok := m.detailCache[p.Key()]; ok && !force && m.detailVersions[p.Key()] == version(p) {
		cached.PR = p // Fresh catalog metadata wins over the prose cache.
		m.detail = &cached
		m.detailStale = false
		m.clampDetail()
		return m.renderDetail()
	}
	m.detailLoading, m.detailDebouncing = true, true
	m.detailStale = m.detail != nil
	token := m.detailToken
	key := p.Key()
	return tea.Tick(previewDelay, func(time.Time) tea.Msg {
		return previewMsg{token: token, key: key, force: force}
	})
}

// Fullscreen is navigation, not refresh. Promote a pending debounce, but retain
// accepted prose or an already-running request for the same catalog revision.
func (m *Model) enterFull(p prs.PullRequest) tea.Cmd {
	m.invalidateAction()
	m.inDetail, m.previewFocused = true, false
	if m.detailKey == p.Key() && m.detailVersion == version(p) {
		if m.detailDebouncing {
			return m.fetchDetail(p)
		}
		if m.detailLoading || m.detail != nil && !m.forceDetail {
			return m.renderDetail()
		}
	}
	if m.detailKey != p.Key() {
		force := m.forceDetail
		m.clearDetail()
		m.forceDetail = force // Explicit refresh still bypasses backend TTLs.
	}
	if cached, ok := m.detailCache[p.Key()]; ok && !m.forceDetail && m.detailVersions[p.Key()] == version(p) {
		cached.PR = p
		m.detail, m.detailKey, m.detailVersion = &cached, p.Key(), version(p)
		m.detailErr, m.detailStale = "", false
		return m.renderDetail()
	}
	return m.fetchDetail(p)
}

// A wide return can retain detail focus/scroll. A narrow return hides details,
// so cancel and invalidate its request even if a late same-key result arrives.
func (m *Model) normalView(previewFocus bool) {
	wasFull := m.inDetail
	m.inDetail = false
	m.previewFocused = previewFocus && m.split()
	if wasFull && !m.split() {
		m.invalidateDetail()
	}
	m.keepVisible()
}

func (m *Model) cycleGroup() tea.Cmd {
	selected, _ := m.selected()
	groups := [...]prs.Group{"", prs.GroupDone, prs.GroupInReview, prs.GroupInProgress, prs.GroupCancelled}
	for i, group := range groups {
		if m.groupFilter == group {
			m.groupFilter = groups[(i+1)%len(groups)]
			break
		}
	}
	m.invalidateAction()
	m.normalView(false)
	m.rank(selected.Key(), false)
	return m.syncPreview(false)
}

func (m *Model) editQuery(query string) tea.Cmd {
	m.query = query
	m.invalidateAction()
	m.normalView(false)
	m.rank("", false)
	return m.syncPreview(false)
}

func (m *Model) fetchDetail(p prs.PullRequest) tea.Cmd {
	return m.fetchDetailForce(p, m.forceDetail)
}

func (m *Model) fetchDetailForce(p prs.PullRequest, force bool) tea.Cmd {
	m.invalidateDetail()
	if m.detail != nil && m.detail.PR.Key() != p.Key() {
		m.detail = nil
	}
	m.detailStale = m.detail != nil
	m.detailKey = p.Key()
	m.detailVersion = version(p)
	m.forceDetail = force
	m.detailLoading = true
	m.detailErr = ""
	if p.IsLocal() {
		m.detailLoading, m.detailStale, m.forceDetail = false, false, false
		m.detail = &prs.Detail{PR: cloneRenderDetail(prs.Detail{PR: p}).PR}
		return m.renderDetail()
	}
	token := m.detailToken
	ctx, cancel := context.WithCancel(m.ctx)
	m.detailCancel = cancel
	p = cloneRenderDetail(prs.Detail{PR: p}).PR
	if fn := m.actions.DetailStream; fn != nil {
		return streamCommand(ctx, cancel, func(emit func(tea.Msg)) error {
			return fn(ctx, p, force, func(update prs.DetailUpdate) {
				update.Detail = cloneRenderDetail(update.Detail)
				emit(detailUpdateMsg{token, p.Key(), update})
			})
		}, func(err error) tea.Msg { return detailMsg{token: token, key: p.Key(), err: err, completeOnly: true} })
	}
	fn := m.actions.Detail
	return func() tea.Msg {
		defer cancel()
		if err := ctx.Err(); err != nil {
			return detailMsg{token: token, key: p.Key(), err: err}
		}
		if fn == nil {
			return detailMsg{token: token, key: p.Key(), err: fmt.Errorf("details unavailable")}
		}
		detail, err := fn(ctx, p)
		return detailMsg{token: token, key: p.Key(), detail: detail, err: err}
	}
}

func (m *Model) invalidateAction() {
	if m.actionCancel != nil {
		m.actionCancel()
	}
	m.actionToken++
	m.actionPending = false
}

func (m *Model) act(p prs.PullRequest, jump bool) tea.Cmd {
	m.invalidateAction()
	if !jump && (p.IsLocal() || p.URL == "") {
		return nil
	}
	m.actionPending = true
	m.err = ""
	token := m.actionToken
	ctx, cancel := context.WithCancel(m.ctx)
	m.actionCancel = cancel
	link, linked := p.PreferredLink()
	jumpFn, openFn := m.actions.Jump, m.actions.OpenURL
	return func() tea.Msg {
		defer cancel()
		if err := ctx.Err(); err != nil {
			return actionMsg{token: token, jump: jump, err: err}
		}
		var err error
		if jump {
			if !linked || jumpFn == nil {
				err = fmt.Errorf("jump unavailable")
			} else {
				err = jumpFn(ctx, link)
			}
		} else {
			if p.URL == "" || openFn == nil {
				err = fmt.Errorf("browser unavailable")
			} else {
				err = openFn(ctx, p.URL)
			}
		}
		return actionMsg{token: token, jump: jump, err: err}
	}
}

func (m *Model) quit() tea.Cmd {
	m.quitting = true
	m.cancel()
	return tea.Quit
}

func (m *Model) selected() (prs.PullRequest, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return prs.PullRequest{}, false
	}
	return m.rows[m.cursor], true
}

func (m *Model) detailPR() (prs.PullRequest, bool) {
	for _, p := range m.catalog.PRs {
		if p.Key() == m.detailKey {
			return p, true
		}
	}
	return prs.PullRequest{}, false
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.quitting {
		return m, nil
	}
	switch msg := msg.(type) {
	case agentSnapshotMsg:
		return m, m.acceptAgentSnapshot(msg)
	case clockMsg:
		if m.ctx.Err() != nil {
			return m, nil
		}
		m.clock = time.Time(msg)
		return m, m.tick()
	case streamMsg:
		_, follow := m.Update(msg.message)
		return m, tea.Batch(follow, msg.next)
	case catalogUpdateMsg:
		if msg.token != m.loadToken {
			return m, nil
		}
		return m, m.applyCatalog(msg.update)
	case detailUpdateMsg:
		if msg.token != m.detailToken || !(m.inDetail || m.split()) || msg.key != m.detailKey {
			return m, nil
		}
		return m, m.applyDetail(msg.key, msg.update)
	case tea.WindowSizeMsg:
		wasSplit := m.split()
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		if !m.split() {
			m.previewFocused = false
			if wasSplit && !m.inDetail {
				m.clearDetail()
			}
		}
		m.keepVisible()
		m.clampDetail()
		return m, tea.Batch(m.syncPreview(false), m.renderDetail())
	case renderedDetailMsg:
		if msg.epoch != m.renderEpoch || m.renderPending[msg.key] == nil {
			return m, nil
		}
		m.renderPending[msg.key]()
		delete(m.renderPending, msg.key)
		m.renderedDetails[msg.key] = msg.lines
		if m.detail != nil && msg.key == m.renderKey(m.detailWidth()) {
			m.frontKey, m.frontLines = msg.key, msg.lines
			m.frameLines = nil
		}
		m.clampDetail()
	case previewMsg:
		p, ok := m.selected()
		if msg.token != m.detailToken || msg.key != m.detailKey || !ok || p.Key() != msg.key || !m.split() {
			return m, nil
		}
		return m, m.fetchDetailForce(p, msg.force || m.forceDetail)
	case loadMsg:
		if msg.token != m.loadToken {
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.err, m.stale = "Refresh: "+msg.err.Error(), m.loaded
			// Retain rows and same-PR detail; a wide preview can retry details.
			return m, tea.Batch(m.syncActiveDetail(), m.renderDetail())
		}
		if msg.completeOnly {
			return m, nil
		}
		return m, m.applyCatalog(prs.CatalogUpdate{Catalog: msg.catalog})
	case detailMsg:
		if msg.token != m.detailToken || !(m.inDetail || m.split()) || msg.key != m.detailKey {
			return m, nil
		}
		m.detailLoading = false
		if msg.err != nil {
			m.detailErr = "Details: " + msg.err.Error()
			m.detailStale = m.detail != nil
			return m, nil
		}
		m.forceDetail = false
		if msg.completeOnly {
			return m, nil
		}
		return m, m.applyDetail(msg.key, prs.DetailUpdate{Detail: msg.detail})
	case actionMsg:
		if msg.token != m.actionToken {
			return m, nil
		}
		m.actionPending = false
		if msg.err != nil {
			m.err = "Action: " + msg.err.Error()
			return m, nil
		}
		if msg.jump {
			return m, m.quit()
		}
	case tea.KeyMsg:
		key := msg.String()
		switch key {
		case "ctrl+c":
			return m, m.quit()
		case "shift+tab":
			return m, m.cycleGroup()
		case "tab":
			if m.split() {
				m.previewFocused = !m.previewFocused
			}
		case "esc", "backspace", "ctrl+h":
			if key != "esc" && m.query != "" {
				r := []rune(m.query)
				return m, m.editQuery(string(r[:len(r)-1]))
			}
			if m.inDetail {
				m.clearDetail()
				m.invalidateAction()
				m.inDetail = false
				m.keepVisible()
				return m, m.syncPreview(false)
			} else if m.previewFocused {
				m.previewFocused = false
			} else {
				return m, m.quit()
			}
		case "ctrl+r":
			m.invalidateAction()
			return m, m.load(true)
		case "ctrl+d":
			if m.inDetail {
				m.invalidateAction()
				m.normalView(true)
				return m, m.syncPreview(false)
			}
			if p, ok := m.selected(); ok {
				return m, m.enterFull(p)
			}
		case "ctrl+o":
			p, ok := m.selected()
			if m.inDetail {
				p, ok = m.detailPR()
			}
			if ok {
				return m, m.act(p, false)
			}
		case "enter":
			if !m.inDetail && !m.previewFocused {
				if p, ok := m.selected(); ok {
					if _, linked := p.PreferredLink(); linked {
						return m, m.act(p, true)
					}
					return m, m.enterFull(p)
				}
			}
		case "up", "down", "pgup", "pgdown", "home", "end":
			if m.inDetail || m.previewFocused {
				m.scrollDetail(key)
			} else {
				m.move(key)
				return m, m.syncPreview(false)
			}
		default:
			if msg.Type == tea.KeyRunes && !msg.Alt {
				return m, m.editQuery(m.query + singleLine(string(msg.Runes)))
			} else if msg.Type == tea.KeySpace && !msg.Alt {
				return m, m.editQuery(m.query + " ")
			}
		}
	}
	return m, nil
}

func current(p prs.PullRequest) bool {
	for _, link := range p.Links {
		if link.Current {
			return true
		}
	}
	return false
}

func searchText(p prs.PullRequest) string {
	s := p.SearchText()
	for _, link := range p.Links {
		s += " " + link.WorkspaceID + " " + link.TabID + " " + link.PaneID
		if link.Current {
			s += " current"
		}
		if link.Agent {
			s += " agent"
		}
	}
	return strings.ToLower(singleLine(s))
}

// Each search term is an independent subsequence; compact matches rank first.
func fuzzyScore(text, query string) (int, bool) {
	haystack := []rune(strings.ToLower(text))
	score := 0
	for _, term := range strings.Fields(strings.ToLower(query)) {
		previous, pos := -1, 0
		for _, want := range []rune(term) {
			for pos < len(haystack) && haystack[pos] != want {
				pos++
			}
			if pos == len(haystack) {
				return 0, false
			}
			if previous < 0 {
				score += pos
			} else {
				score += 4 * (pos - previous - 1)
			}
			previous = pos
			pos++
		}
	}
	return score, true
}

func (m *Model) rank(preserve string, initial bool) {
	type result struct {
		pr    prs.PullRequest
		score int
	}
	matches := make([]result, 0, len(m.catalog.PRs))
	for _, p := range m.catalog.PRs {
		if m.groupFilter != "" && p.Group() != m.groupFilter {
			continue
		}
		if score, ok := fuzzyScore(searchText(p), m.query); ok {
			matches = append(matches, result{p, score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.pr.Group() != b.pr.Group() {
			return a.pr.Group().Order() < b.pr.Group().Order()
		}
		if a.score != b.score {
			return a.score < b.score
		}
		if a.pr.Priority() != b.pr.Priority() {
			return a.pr.Priority() < b.pr.Priority()
		}
		if current(a.pr) != current(b.pr) {
			return current(a.pr)
		}
		return a.pr.Key() < b.pr.Key()
	})
	m.rows = make([]prs.PullRequest, len(matches))
	m.cursor = 0
	for i, match := range matches {
		m.rows[i] = match.pr
	}
	for i, p := range m.rows {
		if p.Key() == preserve || initial && current(p) {
			m.cursor = i
			break
		}
	}
	m.keepVisible()
}

// Reserve one body row even on tiny terminals. Drop decorative dividers first,
// then footer/status/header; View uses exactly the same budget as navigation.
func (m *Model) contentHeight() int {
	chrome := 0
	if m.height >= 2 {
		chrome++ // global search
	}
	if m.height >= 3 {
		chrome++ // status and warnings
	}
	if m.height >= 5 {
		chrome++ // global footer
	}
	if m.height >= 6 {
		chrome += 2 // global dividers
	}
	return max(1, m.height-chrome)
}
func (m *Model) keepVisible() {
	m.cursor = min(max(0, m.cursor), max(0, len(m.rows)-1))
	m.top = groupedList(m.rows).keepVisible(m.top, m.cursor, m.contentHeight())
}
func (m *Model) move(key string) {
	old := m.cursor
	switch key {
	case "up":
		m.cursor--
	case "down":
		m.cursor++
	case "pgup":
		m.cursor = groupedList(m.rows).page(m.cursor, -1, m.contentHeight())
	case "pgdown":
		m.cursor = groupedList(m.rows).page(m.cursor, 1, m.contentHeight())
	case "home":
		m.cursor = 0
	case "end":
		m.cursor = len(m.rows) - 1
	}
	m.keepVisible()
	if old != m.cursor {
		m.invalidateAction()
	}
}
func (m *Model) clampDetail() {
	m.detailTop = min(max(0, m.detailTop), max(0, len(m.detailLines())-m.contentHeight()))
}
func (m *Model) scrollDetail(key string) {
	switch key {
	case "up":
		m.detailTop--
	case "down":
		m.detailTop++
	case "pgup":
		m.detailTop -= m.contentHeight()
	case "pgdown":
		m.detailTop += m.contentHeight()
	case "home":
		m.detailTop = 0
	case "end":
		m.detailTop = len(m.detailLines())
	}
	m.clampDetail()
}

// Strip terminal protocols before filtering controls. Preserve only line breaks
// in prose; bidi formatting is never allowed to alter displayed labels.
func sanitize(s string) string {
	s = ansi.Strip(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case unicode.IsControl(r):
		case r == 0x061c || r == 0x200e || r == 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x206f:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
func singleLine(s string) string     { return strings.ReplaceAll(sanitize(s), "\n", " ") }
func fit(s string, width int) string { return ansi.Truncate(singleLine(s), max(1, width), "…") }

func (m *Model) detailLines() []string { return m.detailLinesWidth(m.detailWidth()) }

func (m *Model) detailLinesWidth(width int) []string {
	if m.detail == nil {
		message := "Details unavailable; Esc to return, Ctrl-R to retry"
		if m.detailLoading {
			// The single global notice covers pending requests, not each pane.
			message = ""
		}
		if m.split() && m.detailKey == "" {
			message = "Select an item"
			if m.loading {
				message = ""
			}
		}
		return []string{fit(message, width)}
	}
	key := m.renderKey(width)
	if lines, ok := m.renderedDetails[key]; ok {
		m.frontKey, m.frontLines = key, lines
		return m.composeDetail(key, lines)
	}
	// Only a complete same-identity, same-width frame may survive refresh.
	if m.frontLines != nil && m.frontKey.identity == key.identity && m.frontKey.width == key.width && m.frontKey.profile == key.profile {
		return m.composeDetail(m.frontKey, m.frontLines)
	}
	if m.renderPending[key] != nil && !m.detail.PR.IsLocal() {
		// First paint waits for complete Markdown instead of briefly showing
		// unstyled prose. Existing complete frames remain visible above.
		return m.composeDetail(key, nil)
	}
	if m.fallbackLines == nil || m.fallbackKey != key {
		m.fallbackKey = key
		// Bound temporary work on the event loop, even for very large prose.
		budget := 16 * 1024
		m.fallbackLines = formatDetailContent(*m.detail, width, func(body string) []string {
			limit := min(len(body), max(0, budget))
			budget -= limit
			text := body[:limit]
			if limit < len(body) {
				text += "\n(Rendering Markdown…)"
			}
			return wrapDetailText(strings.ToValidUTF8(text, ""), width)
		})
	}
	return m.composeDetail(key, m.fallbackLines)
}

// Bubble Tea v1 exposes cursor visibility, not View-relative positioning. Use a
// stationary end cursor instead: inverse space, or a visible bar without ANSI.
// The query is append-only; placeholder text starts AFTER the insertion cell.
func (m *Model) searchHeader() string {
	cursor := "\x1b[97;7m \x1b[0m"
	if m.markdownProfile == termenv.Ascii {
		cursor = "|"
	}
	label := "All"
	if m.groupFilter != "" {
		label = singleLine(string(m.groupFilter))
	}
	// Reserve a usable input plus a two-cell gap. At tiny widths input and
	// caret win over the tag; query truncation always retains its end.
	inputWidth := m.width
	if m.width >= 12+2+ansi.StringWidth(label) {
		inputWidth -= 2 + ansi.StringWidth(label)
	} else {
		label = ""
	}
	prefix := ansi.Truncate("❯ ", max(0, inputWidth-ansi.StringWidth(cursor)), "")
	budget := max(0, inputWidth-ansi.StringWidth(prefix)-ansi.StringWidth(cursor))
	query := singleLine(m.query)
	// Drop whole graphemes from the left, never split a wide/combining cluster
	// or truncate the cursor off the right edge. Input ANSI is sanitized first.
	remaining := 0
	for rest := query; rest != ""; {
		_, tail, cells, _ := ansi.FirstGraphemeCluster(rest, -1)
		rest, remaining = tail, remaining+cells
	}
	for remaining > budget && query != "" {
		_, tail, cells, _ := ansi.FirstGraphemeCluster(query, -1)
		query, remaining = tail, remaining-cells
	}
	header := prefix + query + cursor
	placeholder := ""
	if m.query == "" {
		placeholder = ansi.Truncate(" jump to worktree", max(0, inputWidth-ansi.StringWidth(header)), "")
	}
	gap := ""
	if label != "" {
		gap = strings.Repeat(" ", max(2, m.width-ansi.StringWidth(header+placeholder)-ansi.StringWidth(label)))
	}
	if m.markdownProfile == termenv.Ascii {
		return header + placeholder + gap + label
	}
	// Style each segment independently: neither cyan nor faint reaches the
	// insertion cell, and placeholder styling never leaks into typed input.
	return lipgloss.NewStyle().Foreground(lipgloss.ANSIColor(6)).Render(prefix+query) + cursor +
		lipgloss.NewStyle().Foreground(lipgloss.ANSIColor(8)).Faint(true).Render(placeholder) + gap +
		lipgloss.NewStyle().Faint(true).Render(label)
}

// Errors precede the count so they never disappear behind a long subtitle.
// Catalog warnings remain visible in fullscreen details too.
func (m *Model) notices() string {
	var parts []string
	if m.err != "" {
		parts = append(parts, m.err)
	}
	if m.agentErr != "" {
		parts = append(parts, m.agentErr)
	}
	if (m.inDetail || m.split()) && m.detailErr != "" {
		parts = append(parts, m.detailErr)
	}
	rendering := m.detail != nil && m.renderPending[m.renderKey(m.detailWidth())] != nil
	if m.loading || (m.inDetail || m.split()) && (m.detailLoading && !m.detailDebouncing || rendering) {
		parts = append(parts, "Loading…")
	}
	if m.actionPending {
		parts = append(parts, "Working…")
	}
	parts = append(parts, displayWarnings(m.catalog.Warnings)...)
	return strings.Join(parts, " · ")
}

func (m *Model) footer() string {
	if m.inDetail {
		return "Details · ^D back · ^R refresh · ⇧Tab groups · Esc/Back back · ↑↓ Pg scroll · ^O web"
	}
	if m.split() {
		if m.previewFocused {
			return "Details · Tab/Esc list · ⇧Tab groups · ↑↓ Pg scroll · ^O web · ^R refresh · ^D full"
		}
		return "List · Enter jump/details · Tab preview · ^D full · ⇧Tab groups · ^O web · ^R refresh · ↑↓ Pg move · Esc exit"
	}
	return "List · Enter jump/details · ^D full · ^R refresh · ⇧Tab groups · ↑↓ move · Esc/Back exit · ^O GitHub"
}

func (m *Model) View() string {
	muted := lipgloss.NewStyle().Faint(true)
	var lines []string
	if m.height >= 2 {
		lines = append(lines, m.searchHeader())
	}
	divider := muted.Render(strings.Repeat("─", m.width))
	if m.height >= 6 {
		lines = append(lines, divider)
	}
	notices := m.notices()
	if m.height >= 3 {
		status := fmt.Sprintf("%d/%d items", len(m.rows), len(m.catalog.PRs))
		if notices != "" {
			status = notices + " · " + status
		}
		lines = append(lines, muted.Render(fit(status, m.width)))
	}
	var body []string
	if m.split() {
		leftWidth, rightWidth := m.paneWidths()
		body = m.paneBody(leftWidth, false)
		right := m.paneBody(rightWidth, true)
		separator := muted.Render("│")
		for i := range body {
			body[i] += strings.Repeat(" ", max(0, leftWidth-ansi.StringWidth(body[i]))) + separator + right[i]
		}
	} else {
		body = m.paneBody(m.width, m.inDetail)
	}
	// At one/two rows there is no subtitle. Prefer state over body content;
	// never let decorative chrome hide an error/loading warning.
	if m.height < 3 && notices != "" {
		body[0] = muted.Render(fit(notices, m.width))
	}
	lines = append(lines, body...)
	if m.height >= 6 {
		lines = append(lines, divider)
	}
	if m.height >= 5 {
		lines = append(lines, muted.Render(fit(m.footer(), m.width)))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) paneBody(width int, details bool) []string {
	var body []string
	if details {
		all := m.detailLinesWidth(width)
		top := min(m.detailTop, max(0, len(all)-m.contentHeight()))
		body = append(body, all[top:min(len(all), top+m.contentHeight())]...)
	} else if len(m.rows) == 0 {
		message := "No matching items"
		if m.loading {
			message = ""
		} else if m.err != "" {
			message = "Pull requests unavailable; Ctrl-R to retry"
		}
		body = append(body, fit(message, width))
	} else {
		layout := groupedList(m.rows)
		columns := listRowLayout(m.rows, width, m.clock)
		for _, block := range layout.window(m.top, m.contentHeight()) {
			i := block.item
			if block.heading {
				group := layout.group(i)
				body = append(body, groupHeading(group, width))
			}
			body = append(body, columns.render(m.rows[i], m.clock, i == m.cursor))
		}
	}
	for len(body) < m.contentHeight() {
		body = append(body, "")
	}
	return body
}
