package prs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Runner func(context.Context, ...string) (string, error)

func RunCommand(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("%s: %s: %w", args[0], message, err)
	}
	return strings.TrimSpace(string(out)), nil
}

type Pane struct {
	ID            string      `json:"pane_id"`
	WorkspaceID   string      `json:"workspace_id"`
	TabID         string      `json:"tab_id"`
	CWD           string      `json:"cwd"`
	ForegroundCWD string      `json:"foreground_cwd"`
	Agent         string      `json:"agent"`
	AgentStatus   AgentStatus `json:"agent_status"`
}

func (p Pane) Directory() string {
	if p.ForegroundCWD != "" {
		return p.ForegroundCWD
	}
	return p.CWD
}

type Workspace struct {
	ID          string      `json:"workspace_id"`
	Label       string      `json:"label"`
	AgentStatus AgentStatus `json:"agent_status,omitempty"`
	Worktree    *struct {
		CheckoutPath string `json:"checkout_path"`
	} `json:"worktree"`
}
type Tab struct {
	ID          string      `json:"tab_id"`
	WorkspaceID string      `json:"workspace_id"`
	Label       string      `json:"label"`
	AgentStatus AgentStatus `json:"agent_status,omitempty"`
}

// AgentInfo entries are recognized agents, not ordinary shell panes. IDs must
// agree with a live Pane before their presence or status is used.
type AgentInfo struct {
	WorkspaceID string      `json:"workspace_id"`
	TabID       string      `json:"tab_id"`
	PaneID      string      `json:"pane_id"`
	Agent       string      `json:"agent"`
	AgentStatus AgentStatus `json:"agent_status"`
}

type Snapshot struct {
	FocusedPaneID      string      `json:"focused_pane_id"`
	FocusedWorkspaceID string      `json:"focused_workspace_id"`
	Panes              []Pane      `json:"panes"`
	Workspaces         []Workspace `json:"workspaces"`
	Tabs               []Tab       `json:"tabs"`
	Agents             []AgentInfo `json:"agents"`
}

type Herdr struct {
	SocketPath string
	Binary     string
	Runner     Runner
}

func NewHerdr() *Herdr {
	binary := os.Getenv("HERDR_BIN_PATH")
	if binary == "" {
		binary = "herdr"
	} else if info, err := os.Stat(binary); err != nil || info.Mode()&0111 == 0 {
		binary = "herdr"
	}
	return &Herdr{SocketPath: os.Getenv("HERDR_SOCKET_PATH"), Binary: binary, Runner: RunCommand}
}

func (h *Herdr) socketCall(ctx context.Context, method string, params any) ([]byte, error) {
	if h.SocketPath == "" {
		return nil, errors.New("HERDR_SOCKET_PATH is not set")
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", h.SocketPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	payload, err := json.Marshal(map[string]any{"id": "herdr-prs", "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if _, err = conn.Write(append(payload, '\n')); err != nil {
		return nil, err
	}
	data, err := bufio.NewReader(io.LimitReader(conn, (8<<20)+1)).ReadBytes('\n')
	if len(data) > 8<<20 {
		return nil, errors.New("Herdr response exceeded 8 MiB")
	}
	return data, err
}

func (h *Herdr) call(ctx context.Context, method string, params any, fallback []string, result any) error {
	data, err := h.socketCall(ctx, method, params)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Socket deadlines may fire just before the context timer publishes Err.
	// Never retry a request whose caller deadline has already elapsed.
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	if err != nil {
		if fallback == nil {
			return fmt.Errorf("cannot reach Herdr: %w", err)
		}
		text, runErr := h.Runner(ctx, append([]string{h.Binary}, fallback...)...)
		if runErr != nil {
			return runErr
		}
		data = []byte(text)
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err = json.Unmarshal(data, &response); err != nil {
		return fmt.Errorf("invalid Herdr response: %w", err)
	}
	if response.Error != nil {
		return fmt.Errorf("Herdr: %s", response.Error.Message)
	}
	if len(response.Result) == 0 || string(response.Result) == "null" {
		return errors.New("Herdr returned no result")
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response.Result, result)
}

func (h *Herdr) Snapshot(ctx context.Context) (Snapshot, error) {
	var result struct {
		Snapshot *Snapshot `json:"snapshot"`
	}
	err := h.call(ctx, "session.snapshot", map[string]any{}, []string{"api", "snapshot"}, &result)
	if err != nil {
		return Snapshot{}, err
	}
	if result.Snapshot == nil {
		return Snapshot{}, errors.New("Herdr returned no session snapshot")
	}
	return *result.Snapshot, nil
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func (h *Herdr) validateCheckout(ctx context.Context, cwd string, link Link) error {
	if link.Path == "" {
		return nil
	}
	if cwd == "" {
		return errors.New("linked target has no checkout; refresh the list")
	}
	root, err := h.Runner(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if !samePath(root, link.Path) {
		return errors.New("linked target changed checkout; refresh the list")
	}
	if link.LocalBranch != "" {
		branch, err := h.Runner(ctx, "git", "-C", cwd, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil {
			return err
		}
		if branch != link.LocalBranch {
			return errors.New("linked target changed branch; refresh the list")
		}
	}
	return nil
}

func (h *Herdr) Jump(ctx context.Context, link Link) error {
	snapshot, err := h.Snapshot(ctx)
	if err != nil {
		return err
	}
	var workspace *Workspace
	for i := range snapshot.Workspaces {
		if snapshot.Workspaces[i].ID == link.WorkspaceID {
			workspace = &snapshot.Workspaces[i]
			break
		}
	}
	if workspace == nil {
		return errors.New("worktree workspace closed; refresh the list")
	}
	if link.PaneID != "" {
		var pane *Pane
		for i := range snapshot.Panes {
			if snapshot.Panes[i].ID == link.PaneID {
				pane = &snapshot.Panes[i]
				break
			}
		}
		if pane == nil || pane.WorkspaceID != link.WorkspaceID || pane.TabID != link.TabID {
			return errors.New("linked pane moved or closed; refresh the list")
		}
		if link.Agent && pane.Agent == "" {
			return errors.New("linked agent is no longer running; refresh the list")
		}
		if err = h.validateCheckout(ctx, pane.Directory(), link); err != nil {
			return err
		}
		if link.Agent {
			return h.call(ctx, "agent.focus", map[string]string{"target": link.PaneID}, []string{"agent", "focus", link.PaneID}, nil)
		}
		return h.call(ctx, "pane.focus", map[string]string{"pane_id": link.PaneID}, nil, nil)
	}
	if link.Path != "" {
		if workspace.Worktree == nil {
			return errors.New("workspace checkout changed; refresh the list")
		}
		if err = h.validateCheckout(ctx, workspace.Worktree.CheckoutPath, link); err != nil {
			return err
		}
	}
	if link.TabID != "" {
		exists := false
		for _, tab := range snapshot.Tabs {
			if tab.ID == link.TabID && tab.WorkspaceID == link.WorkspaceID {
				exists = true
				break
			}
		}
		if !exists {
			return errors.New("linked tab closed; refresh the list")
		}
		return h.call(ctx, "tab.focus", map[string]string{"tab_id": link.TabID}, []string{"tab", "focus", link.TabID}, nil)
	}
	return h.call(ctx, "workspace.focus", map[string]string{"workspace_id": link.WorkspaceID}, []string{"workspace", "focus", link.WorkspaceID}, nil)
}

func (h *Herdr) OpenPopup(ctx context.Context) error {
	if os.Getenv("HERDR_ENV") != "1" {
		return errors.New("open herdr-prs from inside Herdr")
	}
	snapshot, err := h.Snapshot(ctx)
	if err != nil {
		return err
	}
	// Herdr popups always attach to the active pane, not the caller's inherited
	// pane. Capture that same focus for initial selection and omit target_pane_id.
	origin := snapshot.FocusedPaneID
	workspace := snapshot.FocusedWorkspaceID
	params := map[string]any{"plugin_id": "herdr-prs", "entrypoint": "picker", "placement": "popup", "focus": true,
		"env": map[string]string{"HERDR_PRS_ORIGIN_PANE": origin, "HERDR_PRS_ORIGIN_WORKSPACE": workspace}}
	// A popup is a singleton modal, not a pane: its launch returns only ok.
	return h.call(ctx, "plugin.pane.open", params, nil, nil)
}

func GitHubRepository(remote string) string {
	var path string
	if strings.HasPrefix(remote, "git@github.com:") {
		path = strings.TrimPrefix(remote, "git@github.com:")
	} else {
		u, err := url.Parse(remote)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
			return ""
		}
		switch u.Scheme {
		case "ssh", "https", "http", "git":
		default:
			return ""
		}
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	for _, part := range parts {
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
				return ""
			}
		}
	}
	return path
}

type Checkout struct {
	// Branch is the mapped PR head; LocalBranch is the checkout's branch.
	// DefaultBranch is empty unless local remote-HEAD refs identify it.
	Path, Branch, LocalBranch, DefaultBranch, HeadRepository string
	// LocalID is the canonical Git checkout path.
	LocalID, Title string
	Repositories   []string
	Links          []Link
	// LocalTiming is ephemeral; only CachedApp tracks local observations.
	LocalTiming *GroupTiming
}

func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

type Discoverer struct{ Runner Runner }

func (d Discoverer) config(ctx context.Context, root, key string) (string, error) {
	value, err := d.Runner(ctx, "git", "-C", root, "config", "--get", key)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil // Missing optional config, not a transport/read failure.
	}
	return value, err
}

// defaultBranch consults only local remote-HEAD symrefs, including shared refs
// in linked worktrees. Missing or conflicting defaults stay unknown.
func (d Discoverer) defaultBranch(ctx context.Context, root, tracking string, remotes map[string]bool) (string, error) {
	if len(remotes) == 0 {
		return "", nil
	}
	args := []string{"git", "-C", root, "for-each-ref", "--format=%(refname) %(symref)"}
	for remote := range remotes {
		args = append(args, "refs/remotes/"+remote+"/HEAD")
	}
	sort.Strings(args[5:])
	text, err := d.Runner(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("cannot identify default branch: %w", err)
	}
	defaults := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[0], "refs/remotes/") || !strings.HasSuffix(fields[0], "/HEAD") {
			continue
		}
		remote := strings.TrimSuffix(strings.TrimPrefix(fields[0], "refs/remotes/"), "/HEAD")
		prefix := "refs/remotes/" + remote + "/"
		if remotes[remote] && strings.HasPrefix(fields[1], prefix) && fields[1] != fields[0] {
			if branch := strings.TrimPrefix(fields[1], prefix); branch != "" {
				defaults[remote] = branch
			}
		}
	}
	if branch := defaults[tracking]; branch != "" {
		return branch, nil
	}
	if branch := defaults["origin"]; branch != "" {
		return branch, nil
	}
	var branch string
	for _, candidate := range defaults {
		if branch != "" && branch != candidate {
			return "", nil
		}
		branch = candidate
	}
	return branch, nil
}

func (d Discoverer) identify(ctx context.Context, root string) (checkout *Checkout, resultErr error) {
	checkout = &Checkout{Path: root, LocalID: root}
	var defaultErr error
	defer func() {
		resultErr = errors.Join(resultErr, defaultErr)
		if ctx.Err() != nil {
			resultErr = ctx.Err()
		}
	}()
	local, branchErr := d.Runner(ctx, "git", "-C", root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if branchErr == nil {
		checkout.Branch, checkout.LocalBranch = local, local
	}
	text, err := d.Runner(ctx, "git", "-C", root, "remote", "-v")
	if err != nil {
		return checkout, err
	}
	fetch, push := map[string]string{}, map[string]string{}
	remotes := map[string]bool{}
	setPush := func(name, repo string) {
		if previous, ok := push[name]; ok && !strings.EqualFold(previous, repo) {
			push[name] = "" // Multiple destinations are not a trusted head identity.
			return
		}
		push[name] = repo
	}
	repositories := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		remotes[fields[0]] = true
		repo := GitHubRepository(fields[1])
		if repo == "" {
			if fields[2] == "(push)" {
				setPush(fields[0], "") // Never fall back to a GitHub fetch URL.
			}
			continue
		}
		if fields[2] == "(fetch)" {
			fetch[fields[0]] = repo
		} else if fields[2] == "(push)" {
			setPush(fields[0], repo)
		} else {
			continue
		}
		repositories[strings.ToLower(repo)] = repo
	}
	var configErr error
	config := func(key string) string {
		if configErr != nil {
			return ""
		}
		value, err := d.config(ctx, root, key)
		configErr = err
		return value
	}
	var tracking string
	if branchErr == nil {
		tracking = config("branch." + local + ".remote")
	}
	checkout.DefaultBranch, defaultErr = d.defaultBranch(ctx, root, tracking, remotes)
	if ctx.Err() != nil || errors.Is(defaultErr, context.Canceled) || errors.Is(defaultErr, context.DeadlineExceeded) {
		return checkout, nil // The deferred result includes defaultErr.
	}
	if branchErr != nil {
		return checkout, fmt.Errorf("cannot identify branch (detached HEAD?): %w", branchErr)
	}
	if len(fetch) == 0 {
		return checkout, configErr
	}
	pushRemote := config("branch." + local + ".pushRemote")
	if pushRemote == "" {
		pushRemote = config("remote.pushDefault")
	}
	if configErr != nil {
		return checkout, configErr
	}
	origin, tracked := fetch["origin"], fetch[tracking]
	if pushRemote == "" && origin != "" && tracked != "" && !strings.EqualFold(origin, tracked) {
		return checkout, errors.New("ambiguous fork/upstream head; set branch pushRemote or remote.pushDefault; PRs remain unlinked")
	}
	name := pushRemote
	if name == "" {
		if origin != "" {
			name = "origin"
		} else if tracked != "" {
			name = tracking
		} else if len(fetch) == 1 {
			for remote := range fetch {
				name = remote
			}
		}
	}
	head, hasPush := push[name]
	if !hasPush {
		head = fetch[name]
	}
	if head == "" {
		return checkout, errors.New("ambiguous head repository; PRs remain unlinked")
	}
	// Arbitrary push refspecs may rename or target several branches. Refuse to
	// guess their PR identity; explicit support can be added if needed.
	pushSpec := config("remote." + name + ".push")
	pushDefault := config("push.default")
	if configErr != nil {
		return checkout, configErr
	}
	if pushSpec != "" {
		return checkout, errors.New("custom push refspecs are not mapped; PRs remain unlinked")
	}
	branch := local
	if pushDefault != "current" && pushDefault != "matching" && tracked != "" && strings.EqualFold(tracked, head) {
		ref := config("branch." + local + ".merge")
		if configErr != nil {
			return checkout, configErr
		}
		if strings.HasPrefix(ref, "refs/heads/") {
			branch = strings.TrimPrefix(ref, "refs/heads/")
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	repos := make([]string, 0, len(repositories))
	for _, repo := range repositories {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	checkout.Branch, checkout.HeadRepository, checkout.Repositories = branch, head, repos
	return checkout, nil
}

func (d Discoverer) Discover(ctx context.Context, s Snapshot, originPane, originWorkspace, popupPane string) ([]Checkout, []string, error) {
	if d.Runner == nil {
		d.Runner = RunCommand
	}
	tabs := map[string]Tab{}
	spaces := map[string]Workspace{}
	for _, tab := range s.Tabs {
		tabs[tab.ID] = tab
	}
	for _, space := range s.Workspaces {
		spaces[space.ID] = space
	}
	type candidate struct {
		cwd, title string
		link       Link
	}
	var candidates []candidate
	for _, pane := range s.Panes {
		if pane.ID == popupPane && popupPane != "" {
			continue
		}
		title := tabs[pane.TabID].Label
		if title == "" {
			title = pane.Agent
		}
		label := title
		if label == "" {
			label = spaces[pane.WorkspaceID].Label
		}
		candidates = append(candidates, candidate{pane.Directory(), title, Link{WorkspaceID: pane.WorkspaceID, TabID: pane.TabID, PaneID: pane.ID, Agent: pane.Agent != "", Label: label, Current: pane.ID == originPane}})
	}
	for _, space := range s.Workspaces {
		cwd := ""
		if space.Worktree != nil {
			cwd = space.Worktree.CheckoutPath
		}
		if cwd != "" {
			candidates = append(candidates, candidate{cwd, "", Link{WorkspaceID: space.ID, Label: space.Label, Current: originPane == "" && space.ID == originWorkspace}})
		}
	}
	byCWD := map[string]*Checkout{}
	byID := map[string]*Checkout{}
	titles := map[Link]string{}
	var order []string
	var warnings []string
	for _, c := range candidates {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		cwd := c.cwd
		if cwd == "" {
			if space := spaces[c.link.WorkspaceID]; space.Worktree != nil {
				cwd = space.Worktree.CheckoutPath
			}
		}
		if cwd == "" {
			continue
		}
		cwd = canonicalPath(cwd)
		checkout, known := byCWD[cwd]
		if !known {
			root, err := d.Runner(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel")
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, nil, err
			}
			if err != nil {
				if !strings.Contains(strings.ToLower(err.Error()), "not a git repository") {
					warnings = append(warnings, fmt.Sprintf("checkout %s: %v", cwd, err))
				}
			} else {
				root = canonicalPath(root)
				checkout = byID[root]
				if checkout == nil {
					checkout, err = d.identify(ctx, root)
					if ctx.Err() != nil {
						return nil, nil, ctx.Err()
					}
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return nil, nil, err
					}
					if err != nil {
						warnings = append(warnings, fmt.Sprintf("%s: %v", root, err))
					}
					byID[root] = checkout
					order = append(order, root)
				}
			}
			byCWD[cwd] = checkout
		}
		if checkout == nil {
			continue
		}
		link := c.link
		link.snapshotDirectory = c.cwd
		link.snapshotCheckout = checkout.Path
		link.snapshotExcludedPane = popupPane
		if space := spaces[link.WorkspaceID]; space.Worktree != nil {
			if link.snapshotDirectory == "" {
				link.snapshotDirectory = space.Worktree.CheckoutPath
			}
			if link.PaneID == "" {
				link.snapshotWorkspacePath = space.Worktree.CheckoutPath
			}
		}
		if c.cwd != "" || link.PaneID == "" {
			link.Path, link.LocalBranch = checkout.Path, checkout.LocalBranch
		}
		checkout.Links = appendLink(checkout.Links, link)
		titles[link] = c.title
	}
	var checkouts []Checkout
	current := false
	for _, id := range order {
		checkout := byID[id]
		if link, ok := (PullRequest{Links: checkout.Links}).PreferredLink(); ok {
			checkout.Title = titles[link]
		}
		checkouts = append(checkouts, *checkout)
		for _, link := range checkout.Links {
			current = current || link.Current
		}
	}
	if !current && originWorkspace != "" {
		for i := range checkouts {
			for j := range checkouts[i].Links {
				link := &checkouts[i].Links[j]
				link.Current = link.WorkspaceID == originWorkspace && link.PaneID == ""
			}
		}
	}
	// Reuse this discovery's snapshot, after all checkout paths and origin
	// markers are established. Agent polling never repeats Git discovery.
	for i := range checkouts {
		checkouts[i].Links = WithAgentSnapshot(PullRequest{Links: checkouts[i].Links}, s).Links
	}
	return checkouts, unique(warnings), nil
}

func appendLink(links []Link, link Link) []Link {
	for _, existing := range links {
		if existing == link {
			return links
		}
	}
	return append(links, link)
}
func unique(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
