package prs

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestAgentSnapshotWireVersionsAndNullableFields(t *testing.T) {
	for _, tt := range []struct {
		name, snapshot string
		want           AgentStatus
	}{
		{"native", `{"version":99,"panes":[{"pane_id":"p","workspace_id":"w","agent":null,"cwd":null,"foreground_cwd":null,"agent_status":"done"}],"agents":[{"pane_id":"p","workspace_id":"w","agent":null,"agent_status":"done","completion_seq":123}],"workspaces":[{"workspace_id":"w","agent_status":"done","worktree":null}],"tabs":[{"tab_id":"t","workspace_id":"w","agent_status":"done"}]}`, AgentDone},
		{"native unknown", `{"panes":[{"pane_id":"p","workspace_id":"w","agent":null,"agent_status":"unknown"}],"agents":[{"pane_id":"p","workspace_id":"w","agent":null,"agent_status":"unknown"}]}`, AgentUnknown},
		{"legacy agent", `{"panes":[{"pane_id":"p","workspace_id":"w","agent":"helper"}]}`, AgentUnknown},
		{"legacy shell", `{"panes":[{"pane_id":"p","workspace_id":"w","agent":null,"agent_status":"unknown"}],"workspaces":[{"workspace_id":"w","agent_status":"unknown"}]}`, AgentNone},
		{"future state", `{"panes":[{"pane_id":"p","workspace_id":"w","agent_status":"future"}],"agents":[{"pane_id":"p","workspace_id":"w","agent":"helper","agent_status":"future"}]}`, AgentUnknown},
		{"null state", `{"panes":[{"pane_id":"p","workspace_id":"w","agent":"helper","agent_status":null}],"agents":null}`, AgentUnknown},
		{"modern empty agents", `{"panes":[{"pane_id":"p","workspace_id":"w","agent":"stale","agent_status":"blocked"}],"agents":[]}`, AgentNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := herdrServer(t, func(c net.Conn, _ herdrRequest, _ <-chan struct{}) {
				_, _ = fmt.Fprintf(c, "{\"result\":{\"snapshot\":%s}}\n", tt.snapshot)
			})
			h := &Herdr{SocketPath: server.path, Runner: herdrNoCLI(t)}
			s, err := h.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			p := WithAgentSnapshot(PullRequest{Links: []Link{{WorkspaceID: "w", PaneID: "p"}}}, s)
			if got := p.AgentStatus(); got != tt.want {
				t.Fatalf("status = %q, want %q; links %+v", got, tt.want, p.Links)
			}
			if calls := server.calls(); len(calls) != 1 || calls[0].Method != "session.snapshot" || len(calls[0].Params) != 0 {
				t.Fatalf("snapshot caused mutations or extra RPC: %+v", calls)
			}
		})
	}
}

func TestAgentStatusPreferredCheckoutAndPriority(t *testing.T) {
	for _, tt := range []struct {
		name  string
		links []Link
		want  AgentStatus
	}{
		{"unlinked", nil, AgentNone},
		{"legacy presence", []Link{{Agent: true}}, AgentUnknown},
		{"shell status is not presence", []Link{{PaneID: "p", AgentStatus: AgentUnknown}}, AgentNone},
		{"same checkout other workspace", []Link{{WorkspaceID: "a", PaneID: "shell", Path: "/one", Current: true}, {WorkspaceID: "b", PaneID: "agent", Path: "/one", Agent: true, AgentStatus: AgentDone}}, AgentDone},
		{"preferred current checkout", []Link{{WorkspaceID: "a", PaneID: "shell", Path: "/one", Current: true}, {WorkspaceID: "b", PaneID: "agent", Path: "/two", Agent: true, AgentStatus: AgentBlocked}}, AgentNone},
		{"same workspace different paths", []Link{{WorkspaceID: "w", PaneID: "idle", Path: "/one", Current: true, Agent: true, AgentStatus: AgentIdle}, {WorkspaceID: "w", PaneID: "blocked", Path: "/two", Agent: true, AgentStatus: AgentBlocked}}, AgentIdle},
		{"legacy workspace isolation", []Link{{WorkspaceID: "a", Current: true}, {WorkspaceID: "b", PaneID: "agent", Agent: true, AgentStatus: AgentBlocked}}, AgentNone},
		{"legacy same workspace", []Link{{WorkspaceID: "a", Current: true}, {WorkspaceID: "a", PaneID: "agent", Agent: true}}, AgentUnknown},
		{"missing workspace identity", []Link{{PaneID: "shell", Current: true}, {PaneID: "agent", Agent: true}}, AgentNone},
		{"pane beats workspace summary", []Link{{WorkspaceID: "w", Path: "/one", Current: true, AgentStatus: AgentBlocked}, {WorkspaceID: "w", PaneID: "agent", Path: "/one", Agent: true, AgentStatus: AgentIdle}}, AgentIdle},
		{"shell allows proven workspace summary", []Link{{WorkspaceID: "w", Path: "/one", Current: true, AgentStatus: AgentBlocked}, {WorkspaceID: "w", PaneID: "shell", Path: "/one"}}, AgentBlocked},
		{"workspace summary", []Link{{WorkspaceID: "w", Path: "/one", AgentStatus: AgentWorking}}, AgentWorking},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := PullRequest{Links: tt.links}
			before := append([]Link(nil), p.Links...)
			if got := p.AgentStatus(); got != tt.want {
				t.Fatalf("status = %q, want %q", got, tt.want)
			}
			if !reflect.DeepEqual(p.Links, before) {
				t.Fatal("aggregate mutated links")
			}
		})
	}
	ordered := []AgentStatus{AgentUnknown, AgentIdle, AgentWorking, AgentDone, AgentBlocked}
	p := PullRequest{Links: []Link{{WorkspaceID: "w", PaneID: "preferred", Path: "/one", Current: true, Agent: true, AgentStatus: AgentIdle}}}
	for i, status := range ordered {
		p.Links = append(p.Links, Link{WorkspaceID: "w", PaneID: fmt.Sprint(i), Path: "/one", Agent: true, AgentStatus: status})
		want := status
		if status == AgentUnknown {
			want = AgentIdle
		}
		if got := p.AgentStatus(); got != want {
			t.Fatalf("priority with %s = %s, want %s", status, got, want)
		}
		preferred, _ := p.PreferredLink()
		if preferred.PaneID != "preferred" {
			t.Fatalf("status changed preferred target: %+v", preferred)
		}
	}
	// Without Current, existing lexical pane preference must still beat urgency.
	p.Links = []Link{{WorkspaceID: "w", PaneID: "a", Path: "/one", Agent: true, AgentStatus: AgentIdle}, {WorkspaceID: "w", PaneID: "z", Path: "/two", Agent: true, AgentStatus: AgentBlocked}}
	if got := p.AgentStatus(); got != AgentIdle {
		t.Fatalf("urgency changed preferred checkout: %s", got)
	}
}

func TestWithAgentSnapshotPresenceAndTopology(t *testing.T) {
	base := Link{WorkspaceID: "w", TabID: "t", PaneID: "p", Path: "/one", Current: true, Agent: true, AgentStatus: AgentDone, LocalBranch: "topic", Label: "origin"}
	for _, tt := range []struct {
		name string
		link Link
		s    Snapshot
		want AgentStatus
	}{
		{"legacy agent", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", Agent: "helper", CWD: "/one"}}}, AgentUnknown},
		{"working", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", Agent: "helper", AgentStatus: AgentWorking, CWD: "/one"}}}, AgentWorking},
		{"strict normalization", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", Agent: "helper", AgentStatus: "WORKING", CWD: "/one"}}}, AgentUnknown},
		{"recognized list", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", CWD: "/one", AgentStatus: AgentBlocked}}, Agents: []AgentInfo{{PaneID: "p", WorkspaceID: "w", AgentStatus: AgentBlocked}}}, AgentBlocked},
		{"agent info fallback", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", CWD: "/one"}}, Agents: []AgentInfo{{PaneID: "p", WorkspaceID: "w", AgentStatus: AgentIdle}}}, AgentIdle},
		{"shell", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", CWD: "/one", AgentStatus: AgentUnknown}}}, AgentNone},
		{"agent exits", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", CWD: "/one", AgentStatus: AgentUnknown}}, Agents: []AgentInfo{}}, AgentNone},
		{"pane exits", base, Snapshot{}, AgentNone},
		{"agent without pane", base, Snapshot{Agents: []AgentInfo{{PaneID: "p", WorkspaceID: "w", AgentStatus: AgentWorking}}}, AgentNone},
		{"moved workspace", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "other", Agent: "helper", CWD: "/one"}}}, AgentNone},
		{"moved tab", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", TabID: "other", Agent: "helper", CWD: "/one"}}}, AgentNone},
		{"moved checkout", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", Agent: "helper", CWD: "/two"}}}, AgentNone},
		{"foreground moved checkout", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", Agent: "helper", CWD: "/one", ForegroundCWD: "/two"}}}, AgentNone},
		{"unproven nested repository", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", Agent: "helper", CWD: "/one/nested"}}}, AgentNone},
		{"missing workspace ID", base, Snapshot{Panes: []Pane{{ID: "p", Agent: "helper", CWD: "/one"}}}, AgentNone},
		{"missing pane ID", base, Snapshot{Panes: []Pane{{WorkspaceID: "w", Agent: "helper", CWD: "/one"}}}, AgentNone},
		{"agent workspace mismatch", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", CWD: "/one"}}, Agents: []AgentInfo{{PaneID: "p", WorkspaceID: "other", AgentStatus: AgentDone}}}, AgentNone},
		{"agent tab mismatch", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", TabID: "t", CWD: "/one"}}, Agents: []AgentInfo{{PaneID: "p", WorkspaceID: "w", TabID: "other", AgentStatus: AgentDone}}}, AgentNone},
		{"duplicate pane identity", base, Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", Agent: "helper", CWD: "/one"}, {ID: "p", WorkspaceID: "w", CWD: "/two"}}}, AgentNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := PullRequest{Links: []Link{tt.link}}
			got := WithAgentSnapshot(p, tt.s)
			if got.AgentStatus() != tt.want || got.Links[0].Agent != (tt.want != AgentNone) {
				t.Fatalf("updated links = %+v; want %s", got.Links, tt.want)
			}
			got.Links[0].Agent, got.Links[0].AgentStatus = tt.link.Agent, tt.link.AgentStatus
			if !reflect.DeepEqual(got, p) {
				t.Fatal("changed non-agent fields", got, p)
			}
		})
	}
	// A previously ordinary shell becomes an agent without Git rediscovery.
	base.Agent, base.AgentStatus = false, AgentNone
	p := WithAgentSnapshot(PullRequest{Links: []Link{base}}, Snapshot{FocusedPaneID: "elsewhere", Panes: []Pane{{ID: "p", WorkspaceID: "w", CWD: "/one", Agent: "helper", AgentStatus: AgentDone}}})
	if p.AgentStatus() != AgentDone || !p.Links[0].Current {
		t.Fatal("new agent or origin lost", p)
	}
}

func TestAgentSnapshotSameWorkspaceDifferentPaths(t *testing.T) {
	s := Snapshot{Workspaces: []Workspace{herdrWorkspace("w", "/one")}, Panes: []Pane{
		{ID: "idle", WorkspaceID: "w", CWD: "/one", Agent: "helper", AgentStatus: AgentIdle},
		{ID: "blocked", WorkspaceID: "w", CWD: "/two", Agent: "helper", AgentStatus: AgentBlocked},
	}}
	s.Workspaces[0].AgentStatus = AgentBlocked
	for _, links := range [][]Link{
		{{WorkspaceID: "w", Path: "/one", Current: true}},
		{{WorkspaceID: "w", Path: "/one", Current: true}, {WorkspaceID: "w", PaneID: "blocked", Path: "/two"}},
		{{WorkspaceID: "w", Path: "/one", Current: true}, {WorkspaceID: "w", PaneID: "idle", Path: "/one"}, {WorkspaceID: "w", PaneID: "blocked", Path: "/two"}},
	} {
		p := WithAgentSnapshot(PullRequest{Links: links}, s)
		if p.AgentStatus() != AgentIdle || p.Links[0].Agent {
			t.Fatalf("workspace borrowed other checkout or became focusable agent: %+v", p.Links)
		}
	}
	// Workspace state cannot manufacture agent presence, even a known status.
	s.Panes = nil
	p := WithAgentSnapshot(PullRequest{Links: []Link{{WorkspaceID: "w", Path: "/one", Current: true, AgentStatus: AgentBlocked}}}, s)
	if p.AgentStatus() != AgentNone {
		t.Fatal("workspace state fabricated presence", p.Links)
	}
	// A contradictory pane link cannot be bypassed through workspace fallback.
	s.Panes = []Pane{{ID: "moved", WorkspaceID: "w", CWD: "/one", Agent: "helper", AgentStatus: AgentBlocked}}
	p = WithAgentSnapshot(PullRequest{Links: []Link{{WorkspaceID: "w", Path: "/one", Current: true}, {WorkspaceID: "w", PaneID: "moved", Path: "/two"}}}, s)
	if p.AgentStatus() != AgentNone {
		t.Fatal("stale topology leaked through summary", p.Links)
	}
}

func TestAgentSnapshotPureAndWorkflowIndependent(t *testing.T) {
	for _, local := range []bool{false, true} {
		p := catalogPR("o/r", 1, "o/r", "topic", "OPEN")
		if local {
			p.Local = &LocalWork{ID: "/one", Path: "/one"}
		}
		p.CreatedAt, p.UpdatedAt, p.CompletedAt = timingCreated, timingChanged, "2026-01-01T00:00:00Z"
		p.GroupTiming = &GroupTiming{Group: p.Group(), Since: timingCreated, Approximate: local}
		p.Links = []Link{{WorkspaceID: "w", PaneID: "p", Path: "/one", Current: true}}
		old := Catalog{PRs: []PullRequest{p}}
		s := Snapshot{FocusedPaneID: "other", Panes: []Pane{{ID: "p", WorkspaceID: "w", CWD: "/one", Agent: "helper", AgentStatus: AgentDone}}, Agents: []AgentInfo{{WorkspaceID: "w", PaneID: "p", AgentStatus: AgentDone}}, Workspaces: []Workspace{herdrWorkspace("w", "/one")}}
		before, _ := json.Marshal(s)
		got := WithAgentSnapshot(p, s)
		after, _ := json.Marshal(s)
		if string(before) != string(after) || old.PRs[0].AgentStatus() != AgentNone || p.AgentStatus() != AgentNone {
			t.Fatal("snapshot or old catalog mutated")
		}
		if got.AgentStatus() != AgentDone || got.Status() != p.Status() || got.Group() != p.Group() || got.Priority() != p.Priority() || got.Key() != p.Key() {
			t.Fatal("agent update changed workflow or identity", got)
		}
		got.Links[0].Agent, got.Links[0].AgentStatus = false, AgentNone
		if !reflect.DeepEqual(got, p) {
			t.Fatal("agent update changed PR fields, clocks or navigation", got)
		}
	}
}

func agentDiscoveryRunner(calls *int) Runner {
	return func(_ context.Context, args ...string) (string, error) {
		*calls++
		switch args[3] {
		case "rev-parse":
			if strings.HasPrefix(args[2], "/one") {
				return "/one", nil
			}
			return "/two", nil
		case "symbolic-ref":
			return "topic", nil
		case "remote", "config":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected command %v", args)
		}
	}
}

func TestDiscoverInitialAgentStatesReuseSnapshotAndGitIdentity(t *testing.T) {
	s := Snapshot{FocusedPaneID: "popup", Workspaces: []Workspace{herdrWorkspace("w", "/one")}, Panes: []Pane{
		{ID: "shell", WorkspaceID: "w", CWD: "/one"},
		{ID: "helper", WorkspaceID: "w", CWD: "/one/sub", Agent: "helper", AgentStatus: AgentDone},
		{ID: "other", WorkspaceID: "w", CWD: "/two", Agent: "helper", AgentStatus: AgentBlocked},
	}}
	var calls int
	d := Discoverer{Runner: agentDiscoveryRunner(&calls)}
	found, warnings, err := d.Discover(context.Background(), s, "shell", "w", "popup")
	if err != nil || len(warnings) != 0 || len(found) != 2 {
		t.Fatal(found, warnings, err)
	}
	initialCalls := calls
	p := PullRequest{Links: found[0].Links}
	if p.AgentStatus() != AgentDone {
		t.Fatal("initial status missing or wrong checkout", p.Links)
	}
	// Subdirectory roots, proven by this discovery, remain usable with no I/O.
	s.Panes[1].AgentStatus = AgentWorking
	p = WithAgentSnapshot(p, s)
	if p.AgentStatus() != AgentWorking || calls != initialCalls {
		t.Fatal("poll lost discovered directory mapping or repeated Git", p.Links, calls)
	}
	for i := range s.Panes {
		s.Panes[i].AgentStatus = AgentNone
	}
	calls = 0
	if _, _, err := d.Discover(context.Background(), s, "shell", "w", "popup"); err != nil || calls != initialCalls {
		t.Fatal("agent status required extra Git discovery", err, calls, initialCalls)
	}
}

func TestDiscoverAgentDirectoryAliasesAndPathlessFallback(t *testing.T) {
	root := herdrGitRepo(t)
	alias := t.TempDir() + "/alias"
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{alias, ""} {
		s := Snapshot{Workspaces: []Workspace{herdrWorkspace("w", alias)}, Panes: []Pane{{ID: "p", WorkspaceID: "w", CWD: directory, Agent: "helper", AgentStatus: AgentIdle}}}
		found, warnings, err := (Discoverer{}).Discover(context.Background(), s, "", "w", "")
		if err != nil || len(warnings) != 0 || len(found) != 1 {
			t.Fatal(found, warnings, err)
		}
		p := WithAgentSnapshot(PullRequest{Links: found[0].Links}, s)
		if p.AgentStatus() != AgentIdle {
			t.Fatal("discovery-proven alias or pathless workspace fallback lost", p.Links)
		}
		// Even a pathless navigation link retains discovery's checkout
		// evidence. Same pane/workspace IDs cannot override a changed root.
		s.Panes[0].CWD = "/different-checkout"
		if moved := WithAgentSnapshot(p, s); moved.AgentStatus() != AgentNone {
			t.Fatal("agent moved checkout through pathless link", moved.Links)
		}
		s.Panes[0].CWD = ""
		s.Workspaces[0].Worktree.CheckoutPath = "/different-checkout"
		if moved := WithAgentSnapshot(p, s); moved.AgentStatus() != AgentNone {
			t.Fatal("workspace moved checkout through pathless link", moved.Links)
		}
	}
}

func TestDiscoverPathlessOriginAndPopupIsolation(t *testing.T) {
	s := Snapshot{Workspaces: []Workspace{herdrWorkspace("w", "/one")}, Panes: []Pane{
		{ID: "origin", WorkspaceID: "w"},
		{ID: "other", WorkspaceID: "w", CWD: "/two", Agent: "helper", AgentStatus: AgentBlocked},
		{ID: "popup", WorkspaceID: "w", CWD: "/one", Agent: "helper", AgentStatus: AgentDone},
	}}
	var calls int
	d := Discoverer{Runner: agentDiscoveryRunner(&calls)}
	found, _, err := d.Discover(context.Background(), s, "origin", "w", "popup")
	if err != nil || len(found) != 2 {
		t.Fatal(found, err)
	}
	var links []Link
	for _, checkout := range found {
		links = append(links, checkout.Links...)
	}
	p := WithAgentSnapshot(PullRequest{Links: links}, s)
	if p.AgentStatus() != AgentNone {
		t.Fatal("pathless origin borrowed other checkout or popup", p.Links)
	}
	preferred, _ := p.PreferredLink()
	if preferred.PaneID != "origin" || preferred.Path != "" {
		t.Fatal("pathless origin navigation changed", preferred)
	}
	// Workspace-only links retain discovery's popup exclusion across polls.
	s.Panes = s.Panes[2:]
	found, _, err = d.Discover(context.Background(), s, "", "w", "popup")
	if err != nil || len(found) != 1 {
		t.Fatal(found, err)
	}
	p = WithAgentSnapshot(PullRequest{Links: found[0].Links}, s)
	if p.AgentStatus() != AgentNone {
		t.Fatal("excluded popup supplied workspace summary", p.Links)
	}
}

func TestAgentSnapshotProvenSummaryWithExistingShell(t *testing.T) {
	s := Snapshot{Workspaces: []Workspace{herdrWorkspace("w", "/one")}, Panes: []Pane{
		{ID: "shell", WorkspaceID: "w", CWD: "/one", AgentStatus: AgentUnknown},
		{ID: "new", WorkspaceID: "w", CWD: "/one", AgentStatus: AgentBlocked},
	}, Agents: []AgentInfo{{WorkspaceID: "w", PaneID: "new", AgentStatus: AgentBlocked}}}
	p := WithAgentSnapshot(PullRequest{Links: []Link{
		{WorkspaceID: "w", PaneID: "shell", Path: "/one", Current: true},
		{WorkspaceID: "w", Path: "/one"},
	}}, s)
	if p.AgentStatus() != AgentBlocked || p.Links[0].Agent || p.Links[1].Agent {
		t.Fatal("proven agent summary lost or shell became agent", p.Links)
	}
}

func TestAppInitialAgentDiscoveryUsesOnlyExistingSnapshot(t *testing.T) {
	s := Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "w", CWD: "/one", AgentStatus: AgentDone}}, Agents: []AgentInfo{{WorkspaceID: "w", PaneID: "p", AgentStatus: AgentDone}}}
	server := herdrSnapshotServer(t, s)
	h := &Herdr{SocketPath: server.path, Runner: herdrNoCLI(t)}
	var calls int
	d := Discoverer{Runner: agentDiscoveryRunner(&calls)}
	app := &App{Herdr: h, Discover: d.Discover, GitHubFactory: func() GitHub { return &catalogGitHub{} }, OriginPane: "p"}
	catalog, err := app.Load(context.Background())
	if err != nil || len(catalog.PRs) != 1 || catalog.PRs[0].AgentStatus() != AgentDone {
		t.Fatal("initial catalog agent status", catalog, err)
	}
	if rpc := server.calls(); len(rpc) != 1 || rpc[0].Method != "session.snapshot" || calls != 4 {
		t.Fatal("extra snapshot or Git queries", rpc, calls)
	}
}

func TestAgentStatusNeverPersistsInRemoteCache(t *testing.T) {
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
	a, now := cacheFixture(t, g)
	scope, _ := a.scope(context.Background(), g)
	p := catalogPR("o/r", 1, "o/r", "topic", "OPEN")
	p.GroupTiming = &GroupTiming{Group: p.Group(), Since: timingCreated}
	p.Links = []Link{{WorkspaceID: "private-w", PaneID: "private-p", Path: "/private-path", Agent: true, AgentStatus: AgentBlocked}}
	for _, record := range []cacheRecord{{Key: "mine", SavedAt: *now, PRs: []PullRequest{p}}, {Key: detailCacheKey(p), SavedAt: *now, Detail: &Detail{PR: p}}} {
		if err := a.writeCache(context.Background(), scope, record); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(a.cachePath(scope, record.Key))
		if err != nil || strings.Contains(string(data), "agent_status") || strings.Contains(string(data), "private-") {
			t.Fatal("ephemeral agent state persisted", string(data), err)
		}
		got, ok := a.readCache(scope, record.Key)
		if !ok || got.SavedAt != *now {
			t.Fatal("cache timing changed", got, ok)
		}
		remote := p
		if got.Detail != nil {
			remote = got.Detail.PR
		} else {
			remote = got.PRs[0]
		}
		if remote.AgentStatus() != AgentNone || len(remote.Links) != 0 || !reflect.DeepEqual(remote.GroupTiming, p.GroupTiming) || !p.Links[0].Agent {
			t.Fatal("cache status isolation failed", remote, p)
		}
	}
}
