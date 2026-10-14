package prs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type herdrRequest struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}
type herdrLocalServer struct {
	path        string
	mu          sync.Mutex
	requests    []herdrRequest
	connections map[net.Conn]bool
	stop        chan struct{}
}

// Unix socket only; never consults a real Herdr socket or executes Herdr.
func herdrServer(t *testing.T, handle func(net.Conn, herdrRequest, <-chan struct{})) *herdrLocalServer {
	t.Helper()
	// Short path: macOS Unix sockets cannot fit long testing.TempDir paths.
	dir, err := os.MkdirTemp("", "prs-sock-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	s := &herdrLocalServer{path: path, connections: map[net.Conn]bool{}, stop: make(chan struct{})}
	var handlers sync.WaitGroup
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.connections[conn] = true
			s.mu.Unlock()
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer conn.Close()
				defer func() { s.mu.Lock(); delete(s.connections, conn); s.mu.Unlock() }()
				data, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					t.Error(err)
					return
				}
				var request herdrRequest
				if err := json.Unmarshal(data, &request); err != nil {
					t.Error(err)
					return
				}
				s.mu.Lock()
				s.requests = append(s.requests, request)
				s.mu.Unlock()
				handle(conn, request, s.stop)
			}()
		}
	}()
	t.Cleanup(func() {
		close(s.stop)
		_ = listener.Close()
		<-accepted
		s.mu.Lock()
		for conn := range s.connections {
			_ = conn.Close()
		}
		s.mu.Unlock()
		handlers.Wait()
		_ = os.RemoveAll(dir)
	})
	return s
}
func (s *herdrLocalServer) calls() []herdrRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]herdrRequest(nil), s.requests...)
}
func herdrNoCLI(t *testing.T) Runner {
	return func(context.Context, ...string) (string, error) {
		t.Error("unexpected Herdr CLI fallback")
		return "", errors.New("CLI forbidden")
	}
}
func herdrEncode(t *testing.T, conn net.Conn, response any) {
	t.Helper()
	if err := json.NewEncoder(conn).Encode(response); err != nil {
		t.Error(err)
	}
}
func herdrSnapshotServer(t *testing.T, snapshot Snapshot) *herdrLocalServer {
	return herdrServer(t, func(conn net.Conn, r herdrRequest, _ <-chan struct{}) {
		if r.Method == "session.snapshot" {
			herdrEncode(t, conn, map[string]any{"result": map[string]any{"snapshot": snapshot}})
		} else {
			herdrEncode(t, conn, map[string]any{"result": map[string]any{"ok": true}})
		}
	})
}

func TestHerdrSnapshotJSONEnvelopeAndUnwrap(t *testing.T) {
	w := herdrWorkspace("ws", "/offline/checkout")
	w.Label, w.AgentStatus = "work", AgentBlocked
	want := Snapshot{FocusedPaneID: "pane", FocusedWorkspaceID: "ws",
		Panes:      []Pane{{ID: "pane", WorkspaceID: "ws", TabID: "tab", CWD: "/offline", ForegroundCWD: "/offline/nested", Agent: "agent", AgentStatus: AgentWorking}},
		Workspaces: []Workspace{w}, Tabs: []Tab{{ID: "tab", WorkspaceID: "ws", Label: "topic", AgentStatus: AgentIdle}},
		Agents: []AgentInfo{{WorkspaceID: "ws", TabID: "tab", PaneID: "pane", Agent: "agent", AgentStatus: AgentDone}}}
	// Literal wire names prevent a production-tag typo passing a self round trip.
	s := herdrServer(t, func(c net.Conn, _ herdrRequest, _ <-chan struct{}) {
		_, _ = fmt.Fprintln(c, `{"result":{"snapshot":{"focused_pane_id":"pane","focused_workspace_id":"ws","panes":[{"pane_id":"pane","workspace_id":"ws","tab_id":"tab","cwd":"/offline","foreground_cwd":"/offline/nested","agent":"agent","agent_status":"working"}],"workspaces":[{"workspace_id":"ws","label":"work","agent_status":"blocked","worktree":{"checkout_path":"/offline/checkout"}}],"tabs":[{"tab_id":"tab","workspace_id":"ws","label":"topic","agent_status":"idle"}],"agents":[{"workspace_id":"ws","tab_id":"tab","pane_id":"pane","agent":"agent","agent_status":"done"}]}}}`)
	})
	h := &Herdr{SocketPath: s.path, Binary: "never-execute", Runner: herdrNoCLI(t)}
	got, err := h.Snapshot(context.Background())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %+v, %v; want %+v", got, err, want)
	}
	if calls := s.calls(); !reflect.DeepEqual(calls, []herdrRequest{{ID: "herdr-prs", Method: "session.snapshot", Params: map[string]any{}}}) {
		t.Fatalf("envelope = %+v", calls)
	}
}

func TestHerdrBusinessAndProtocolErrorsNeverRetry(t *testing.T) {
	for _, tt := range []struct{ name, reply, want string }{
		{"business", `{"error":{"message":"workspace closed"}}`, "Herdr: workspace closed"},
		{"invalid JSON", `not-json`, "invalid Herdr response"},
		{"null result", `{"result":null}`, "Herdr returned no result"},
		{"missing result", `{}`, "Herdr returned no result"},
		{"missing snapshot", `{"result":{}}`, "Herdr returned no session snapshot"},
		{"unwrapped snapshot", `{"result":{"panes":[]}}`, "Herdr returned no session snapshot"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := herdrServer(t, func(c net.Conn, _ herdrRequest, _ <-chan struct{}) { _, _ = fmt.Fprintln(c, tt.reply) })
			h := &Herdr{SocketPath: s.path, Binary: "never-execute", Runner: herdrNoCLI(t)}
			if _, err := h.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if len(s.calls()) != 1 {
				t.Fatal("socket request retried")
			}
		})
	}
}

func TestHerdrTransportFallbackUsesJSONResult(t *testing.T) {
	for _, kind := range []string{"unset socket", "missing socket", "connection closed"} {
		t.Run(kind, func(t *testing.T) {
			path := ""
			switch kind {
			case "missing socket":
				path = filepath.Join(t.TempDir(), "missing.sock")
			case "connection closed":
				path = herdrServer(t, func(net.Conn, herdrRequest, <-chan struct{}) {}).path
			}
			var calls [][]string
			h := &Herdr{SocketPath: path, Binary: "offline-herdr", Runner: func(_ context.Context, args ...string) (string, error) {
				calls = append(calls, append([]string(nil), args...))
				return `{"result":{"snapshot":{"focused_pane_id":"offline"}}}`, nil
			}}
			s, err := h.Snapshot(context.Background())
			if err != nil || s.FocusedPaneID != "offline" || !reflect.DeepEqual(calls, [][]string{{"offline-herdr", "api", "snapshot"}}) {
				t.Fatalf("fallback = %+v %v %v", s, calls, err)
			}
		})
	}
}

func TestHerdrFocusTransportFallback(t *testing.T) {
	for _, tc := range []struct {
		target, id string
		link       Link
	}{
		{"pane", "pane", Link{WorkspaceID: "ws", TabID: "tab", PaneID: "pane"}},
		{"agent", "pane", Link{WorkspaceID: "ws", TabID: "tab", PaneID: "pane", Agent: true}},
		{"tab", "tab", Link{WorkspaceID: "ws", TabID: "tab"}},
		{"workspace", "ws", Link{WorkspaceID: "ws"}},
	} {
		t.Run(tc.target, func(t *testing.T) {
			snapshot := Snapshot{Panes: []Pane{{ID: "pane", WorkspaceID: "ws", TabID: "tab", Agent: "agent"}}, Workspaces: []Workspace{{ID: "ws"}}, Tabs: []Tab{{ID: "tab", WorkspaceID: "ws"}}}
			s := herdrServer(t, func(c net.Conn, r herdrRequest, _ <-chan struct{}) {
				if r.Method == "session.snapshot" {
					herdrEncode(t, c, map[string]any{"result": map[string]any{"snapshot": snapshot}})
				} // Closing the second connection triggers a real transport failure.
			})
			var calls [][]string
			h := &Herdr{SocketPath: s.path, Binary: "offline-herdr", Runner: func(_ context.Context, args ...string) (string, error) {
				calls = append(calls, append([]string(nil), args...))
				return `{"result":{"ok":true}}`, nil
			}}
			err := h.Jump(context.Background(), tc.link)
			if tc.target == "pane" {
				if err == nil || !strings.Contains(err.Error(), "cannot reach Herdr") || len(calls) != 0 {
					t.Fatal("pane focus incorrectly fell back", err, calls)
				}
			} else {
				if err != nil || !reflect.DeepEqual(calls, [][]string{{"offline-herdr", tc.target, "focus", tc.id}}) {
					t.Fatal("incorrect focus fallback", err, calls)
				}
			}
			if requests := s.calls(); len(requests) != 2 || requests[1].Method != tc.target+".focus" {
				t.Fatal("focus transport not exercised", requests)
			}
		})
	}
}

func TestHerdrSocketTimeoutAndCancellationDoNotFallback(t *testing.T) {
	for _, kind := range []string{"deadline", "cancel", "pre-cancel"} {
		t.Run(kind, func(t *testing.T) {
			started := make(chan struct{})
			s := herdrServer(t, func(_ net.Conn, _ herdrRequest, stop <-chan struct{}) { close(started); <-stop })
			h := &Herdr{SocketPath: s.path, Binary: "never-execute", Runner: herdrNoCLI(t)}
			ctx, cancel := context.WithCancel(context.Background())
			want := error(context.Canceled)
			if kind == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 80*time.Millisecond)
				want = context.DeadlineExceeded
			}
			defer cancel()
			if kind == "pre-cancel" {
				cancel()
			}
			done := make(chan error, 1)
			go func() { _, err := h.Snapshot(ctx); done <- err }()
			if kind != "pre-cancel" {
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatal("socket request not received")
				}
			}
			if kind == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("error = %v, want %v", err, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation/timeout did not interrupt socket read")
			}
		})
	}
}

func TestGitHubRepositoryRemoteForms(t *testing.T) {
	for _, remote := range []string{"git@github.com:Owner/repo.git", "https://github.com/Owner/repo.git", "ssh://git@github.com/Owner/repo.git", "ssh://git@github.com:22/Owner/repo.git", "https://GITHUB.COM/Owner/repo/", "git://github.com/Owner/repo", "http://github.com/Owner/repo"} {
		if got := GitHubRepository(remote); got != "Owner/repo" {
			t.Errorf("%q = %q", remote, got)
		}
	}
	for _, remote := range []string{"", "git@gitlab.com:o/r.git", "https://github.com.evil/o/r", "https://github.com/o/r/extra", "https://github.com/o", "https://github.com//r", "https://github.com/o/r%20bad", "file://github.com/o/r", "/tmp/repo", "https://github.com/o/r;bad"} {
		if got := GitHubRepository(remote); got != "" {
			t.Errorf("invalid remote %q = %q", remote, got)
		}
	}
}

func herdrGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := RunCommand(context.Background(), append([]string{"git", "-C", root}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func herdrGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG_PARAMETERS"} {
		catalogUnsetEnv(t, key)
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_COUNT", "0")
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	root = resolved
	herdrGit(t, root, "init", "-b", "local")
	herdrGit(t, root, "-c", "user.name=Offline", "-c", "user.email=offline@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "offline fixture")
	return root
}
func herdrWorkspace(id, root string) Workspace {
	w := Workspace{ID: id, Label: id}
	w.Worktree = &struct {
		CheckoutPath string `json:"checkout_path"`
	}{CheckoutPath: root}
	return w
}

func TestDiscoverGitHeadIdentityAndForkAmbiguity(t *testing.T) {
	for _, tt := range []struct {
		name                                             string
		config                                           [][2]string
		pushurl, tracking, wantHead, wantBranch, warning string
	}{
		{name: "untracked origin", wantHead: "fork/repo", wantBranch: "local"},
		{name: "renamed tracking branch", tracking: "origin", wantHead: "fork/repo", wantBranch: "remote-topic"},
		{name: "current push overrides tracking branch", tracking: "origin", config: [][2]string{{"push.default", "current"}}, wantHead: "fork/repo", wantBranch: "local"},
		{name: "custom push refspec refuses identity guess", tracking: "origin", config: [][2]string{{"remote.origin.push", "refs/heads/local:refs/heads/pr-head"}}, warning: "custom push refspecs"},
		{name: "ambiguous fork upstream", tracking: "upstream", warning: "ambiguous fork/upstream"},
		{name: "branch pushRemote", tracking: "upstream", config: [][2]string{{"branch.local.pushRemote", "origin"}}, wantHead: "fork/repo", wantBranch: "local"},
		{name: "pushDefault", tracking: "upstream", config: [][2]string{{"remote.pushDefault", "origin"}}, wantHead: "fork/repo", wantBranch: "local"},
		{name: "branch overrides default", tracking: "upstream", config: [][2]string{{"remote.pushDefault", "upstream"}, {"branch.local.pushRemote", "origin"}}, wantHead: "fork/repo", wantBranch: "local"},
		{name: "explicit upstream push", tracking: "upstream", config: [][2]string{{"branch.local.pushRemote", "upstream"}}, wantHead: "upstream/repo", wantBranch: "remote-topic"},
		{name: "pushurl identifies destination not fetch", tracking: "origin", pushurl: "git@github.com:push-fork/repo.git", wantHead: "push-fork/repo", wantBranch: "local"},
		{name: "non-GitHub push refuses fetch identity", tracking: "origin", pushurl: "https://gitlab.com/fork/repo.git", warning: "ambiguous head repository"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := herdrGitRepo(t)
			herdrGit(t, root, "remote", "add", "origin", "git@github.com:fork/repo.git")
			herdrGit(t, root, "remote", "add", "upstream", "https://github.com/upstream/repo.git")
			if tt.tracking != "" {
				herdrGit(t, root, "config", "--local", "branch.local.remote", tt.tracking)
				herdrGit(t, root, "config", "--local", "branch.local.merge", "refs/heads/remote-topic")
			}
			if tt.pushurl != "" {
				herdrGit(t, root, "config", "--local", "remote.origin.pushurl", tt.pushurl)
			}
			for _, config := range tt.config {
				herdrGit(t, root, "config", "--local", config[0], config[1])
			}
			s := Snapshot{Panes: []Pane{{ID: "p", WorkspaceID: "ws", CWD: root}}}
			found, warnings, err := (Discoverer{Runner: RunCommand}).Discover(context.Background(), s, "p", "ws", "popup")
			if err != nil {
				t.Fatal(err)
			}
			if tt.warning != "" {
				if len(found) != 1 || found[0].HeadRepository != "" || len(found[0].Repositories) != 0 || found[0].Path != root || found[0].LocalBranch != "local" || !catalogWarning(warnings, tt.warning) {
					t.Fatalf("ambiguous checkout = %+v, warnings %v", found, warnings)
				}
				return
			}
			if len(found) != 1 || len(warnings) != 0 {
				t.Fatalf("found = %+v, warnings %v", found, warnings)
			}
			c := found[0]
			if c.HeadRepository != tt.wantHead || c.Branch != tt.wantBranch || c.LocalBranch != "local" || c.Path != root || len(c.Links) != 1 || c.Links[0].LocalBranch != "local" || !c.Links[0].Current {
				t.Fatalf("checkout = %+v", c)
			}
			wantRepos := []string{"fork/repo", "upstream/repo"}
			if tt.pushurl != "" {
				wantRepos = []string{"fork/repo", "push-fork/repo", "upstream/repo"}
			}
			if !reflect.DeepEqual(c.Repositories, wantRepos) {
				t.Fatalf("repositories = %v, want %v", c.Repositories, wantRepos)
			}
		})
	}
}

func herdrRemoteDefault(t *testing.T, root, remote, branch string) {
	t.Helper()
	ref := "refs/remotes/" + remote + "/" + branch
	herdrGit(t, root, "update-ref", ref, "HEAD")
	herdrGit(t, root, "symbolic-ref", "refs/remotes/"+remote+"/HEAD", ref)
}

func TestDiscoverDefaultBranchFromLocalRemoteHEAD(t *testing.T) {
	type remote struct{ name, branch, url string }
	for _, tt := range []struct {
		name, local, tracking, want, warning string
		remotes                              []remote
	}{
		{name: "GitHub origin", want: "main", remotes: []remote{{"origin", "main", "https://github.com/o/r.git"}}},
		{name: "checked out default", local: "main", want: "main", remotes: []remote{{"origin", "main", "https://github.com/o/r.git"}}},
		{name: "custom default keeps feature named main", local: "main", want: "develop", remotes: []remote{{"origin", "develop", "https://github.com/o/r.git"}}},
		{name: "nested default", want: "release/stable/current", remotes: []remote{{"origin", "release/stable/current", "https://github.com/o/r.git"}}},
		{name: "GitLab without GitHub", want: "develop", remotes: []remote{{"origin", "develop", "https://gitlab.com/o/r.git"}}},
		{name: "no remote is unknown"},
		{name: "missing remote HEAD is unknown", remotes: []remote{{"origin", "", "https://github.com/o/r.git"}}},
		{name: "tracking preferred over origin", tracking: "upstream", want: "develop", warning: "ambiguous fork/upstream head", remotes: []remote{{"origin", "main", "https://github.com/fork/r.git"}, {"upstream", "develop", "https://github.com/o/r.git"}}},
		{name: "unusable tracking falls back to origin", tracking: "upstream", want: "main", remotes: []remote{{"origin", "main", "https://github.com/o/r.git"}, {"upstream", "", "https://github.com/o/r.git"}}},
		{name: "slashed tracking remote", tracking: "team/upstream", want: "release/stable", remotes: []remote{{"origin", "main", "https://github.com/o/r.git"}, {"team/upstream", "release/stable", "https://gitlab.com/o/r.git"}}},
		{name: "slashed sole remote", want: "release/stable", remotes: []remote{{"team/upstream", "release/stable", "https://github.com/o/r.git"}}},
		{name: "sole known default among several remotes", want: "develop", warning: "ambiguous head repository", remotes: []remote{{"a", "", "https://github.com/a/r.git"}, {"b", "develop", "https://github.com/b/r.git"}}},
		{name: "conflicting defaults without authority", warning: "ambiguous head repository", remotes: []remote{{"a", "main", "https://github.com/a/r.git"}, {"b", "develop", "https://github.com/b/r.git"}}},
		{name: "agreeing defaults without authority", want: "develop", warning: "ambiguous head repository", remotes: []remote{{"a", "develop", "https://github.com/a/r.git"}, {"b", "develop", "https://github.com/b/r.git"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := herdrGitRepo(t)
			local := tt.local
			if local == "" {
				local = "local"
			} else {
				herdrGit(t, root, "branch", "-m", local)
			}
			// Neither the init preference nor the current branch is evidence.
			herdrGit(t, root, "config", "--local", "init.defaultBranch", local)
			for _, remote := range tt.remotes {
				herdrGit(t, root, "remote", "add", remote.name, remote.url)
				if remote.branch != "" {
					herdrRemoteDefault(t, root, remote.name, remote.branch)
				}
			}
			if tt.tracking != "" {
				herdrGit(t, root, "config", "--local", "branch."+local+".remote", tt.tracking)
			}
			found, warnings, err := (Discoverer{}).Discover(context.Background(), Snapshot{Panes: []Pane{{ID: "p", CWD: root}}}, "p", "", "")
			if err != nil || len(found) != 1 || found[0].DefaultBranch != tt.want || found[0].LocalBranch != local {
				t.Fatalf("default discovery = %+v, %v, %v; want %q", found, warnings, err, tt.want)
			}
			if tt.warning == "" && len(warnings) != 0 || tt.warning != "" && !catalogWarning(warnings, tt.warning) {
				t.Fatalf("warnings = %v; want %q", warnings, tt.warning)
			}
		})
	}
}

func TestDiscoverDefaultBranchRejectsNonHEADAndCrossRemoteRefs(t *testing.T) {
	for _, kind := range []string{"ordinary HEAD ref", "cross-remote symref", "non-HEAD symref", "unconfigured remote"} {
		t.Run(kind, func(t *testing.T) {
			root := herdrGitRepo(t)
			herdrGit(t, root, "remote", "add", "origin", "https://github.com/o/r.git")
			switch kind {
			case "ordinary HEAD ref":
				herdrGit(t, root, "update-ref", "refs/remotes/origin/HEAD", "HEAD")
			case "cross-remote symref":
				herdrGit(t, root, "update-ref", "refs/remotes/other/main", "HEAD")
				herdrGit(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/other/main")
			case "non-HEAD symref":
				herdrGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
				herdrGit(t, root, "symbolic-ref", "refs/remotes/origin/not-HEAD", "refs/remotes/origin/main")
			case "unconfigured remote":
				herdrRemoteDefault(t, root, "old-remote", "main")
			}
			found, warnings, err := (Discoverer{}).Discover(context.Background(), Snapshot{Panes: []Pane{{ID: "p", CWD: root}}}, "p", "", "")
			if err != nil || len(warnings) != 0 || len(found) != 1 || found[0].DefaultBranch != "" || found[0].HeadRepository != "o/r" {
				t.Fatalf("untrusted default = %+v, %v, %v", found, warnings, err)
			}
		})
	}
}

func TestDiscoverDefaultBranchLinkedWorktreeAndCanonicalDedup(t *testing.T) {
	root := herdrGitRepo(t)
	herdrGit(t, root, "remote", "add", "origin", "https://github.com/o/r.git")
	herdrRemoteDefault(t, root, "origin", "release/stable")
	worktree := filepath.Join(t.TempDir(), "linked")
	herdrGit(t, root, "worktree", "add", "-b", "feature", worktree)
	worktree = canonicalPath(worktree)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(worktree, alias); err != nil {
		t.Fatal(err)
	}
	found, warnings, err := (Discoverer{}).Discover(context.Background(), Snapshot{Panes: []Pane{{ID: "p", CWD: worktree}, {ID: "alias", CWD: alias}}}, "p", "", "")
	if err != nil || len(warnings) != 0 || len(found) != 1 || found[0].Path != worktree || found[0].DefaultBranch != "release/stable" || found[0].LocalBranch != "feature" || len(found[0].Links) != 2 {
		t.Fatalf("linked worktree = %+v, %v, %v", found, warnings, err)
	}
	for _, link := range found[0].Links {
		if link.Path != worktree || link.LocalBranch != "feature" {
			t.Errorf("navigation = %+v", link)
		}
	}
}

func TestDiscoverUnknownDefaultPreservesMappedPRIdentityOffline(t *testing.T) {
	for _, failDefault := range []bool{false, true} {
		t.Run(fmt.Sprint(failDefault), func(t *testing.T) {
			root := herdrGitRepo(t)
			herdrGit(t, root, "remote", "add", "origin", "https://github.com/o/r.git")
			herdrGit(t, root, "config", "--local", "init.defaultBranch", "local")
			herdrGit(t, root, "config", "--local", "branch.local.remote", "origin")
			herdrGit(t, root, "config", "--local", "branch.local.merge", "refs/heads/remote/topic")
			d := Discoverer{Runner: func(ctx context.Context, args ...string) (string, error) {
				if len(args) < 4 || args[0] != "git" {
					t.Fatalf("unexpected command: %v", args)
				}
				switch args[3] {
				case "rev-parse", "symbolic-ref", "config", "for-each-ref":
				case "remote":
					if !reflect.DeepEqual(args[3:], []string{"remote", "-v"}) {
						t.Fatalf("network command forbidden: %v", args)
					}
				default:
					t.Fatalf("network/auth command forbidden: %v", args)
				}
				if args[3] == "for-each-ref" && failDefault {
					return "", errors.New("cannot read refs")
				}
				return RunCommand(ctx, args...)
			}}
			found, warnings, err := d.Discover(context.Background(), Snapshot{Panes: []Pane{{ID: "p", CWD: root}}}, "p", "", "")
			if err != nil || len(found) != 1 || found[0].DefaultBranch != "" || found[0].LocalBranch != "local" || found[0].Branch != "remote/topic" || found[0].HeadRepository != "o/r" || !reflect.DeepEqual(found[0].Repositories, []string{"o/r"}) {
				t.Fatalf("PR identity = %+v, %v, %v", found, warnings, err)
			}
			if failDefault && !catalogWarning(warnings, "cannot identify default branch") || !failDefault && len(warnings) != 0 {
				t.Fatalf("default warnings = %v", warnings)
			}
		})
	}
}

func TestDiscoverDefaultBranchCancellation(t *testing.T) {
	for _, kind := range []string{"caller cancellation", "runner cancellation", "runner deadline"} {
		t.Run(kind, func(t *testing.T) {
			root := herdrGitRepo(t)
			herdrGit(t, root, "remote", "add", "origin", "https://github.com/o/r.git")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(context.Canceled)
			if kind == "runner deadline" {
				want = context.DeadlineExceeded
			}
			d := Discoverer{Runner: func(ctx context.Context, args ...string) (string, error) {
				if args[3] == "for-each-ref" {
					if kind == "caller cancellation" {
						cancel()
					}
					return "", want
				}
				return RunCommand(ctx, args...)
			}}
			if _, _, err := d.Discover(ctx, Snapshot{Panes: []Pane{{ID: "p", CWD: root}}}, "p", "", ""); !errors.Is(err, want) {
				t.Fatalf("default resolution cancellation = %v; want %v", err, want)
			}
		})
	}
}

func TestDiscoverExcludesNonGitAndMissingCWDSpaces(t *testing.T) {
	notGit := t.TempDir()
	s := Snapshot{Workspaces: []Workspace{herdrWorkspace("nongit", notGit), {ID: "empty"}, {ID: "panes"}}, Panes: []Pane{{ID: "p", WorkspaceID: "panes", CWD: notGit}, {ID: "missing", WorkspaceID: "panes"}}}
	found, warnings, err := (Discoverer{}).Discover(context.Background(), s, "p", "panes", "")
	if err != nil || len(found) != 0 || len(warnings) != 0 {
		t.Fatalf("excluded spaces = %+v, %v, %v", found, warnings, err)
	}
	root := herdrGitRepo(t)
	s = Snapshot{Workspaces: []Workspace{herdrWorkspace("git", root)}, Panes: []Pane{{ID: "missing", WorkspaceID: "git"}}}
	found, warnings, err = (Discoverer{}).Discover(context.Background(), s, "", "git", "")
	if err != nil || len(found) != 1 || len(warnings) != 0 || len(found[0].Links) != 2 {
		t.Fatalf("Git metadata fallback = %+v, %v, %v", found, warnings, err)
	}
	for _, link := range found[0].Links {
		if link.PaneID == "" && (link.Path != root || !link.Current) || link.PaneID != "" && link.Path != "" {
			t.Errorf("metadata navigation = %+v", link)
		}
	}
}

func TestDiscoverNestedCheckoutPopupAndCurrentOrigin(t *testing.T) {
	root := herdrGitRepo(t)
	herdrGit(t, root, "remote", "add", "origin", "https://github.com/o/r.git")
	nested := filepath.Join(root, "src", "nested")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	s := Snapshot{FocusedPaneID: "popup", FocusedWorkspaceID: "ws", Workspaces: []Workspace{herdrWorkspace("ws", root)}, Tabs: []Tab{{ID: "tab", WorkspaceID: "ws", Label: "topic tab"}}, Panes: []Pane{
		{ID: "popup", WorkspaceID: "ws", TabID: "tab", CWD: root, Agent: "popup-agent"},
		{ID: "origin", WorkspaceID: "ws", TabID: "tab", CWD: "/not/a/repo", ForegroundCWD: nested},
		{ID: "agent", WorkspaceID: "ws", TabID: "tab", CWD: root, Agent: "agent"},
	}}
	found, warnings, err := (Discoverer{}).Discover(context.Background(), s, "origin", "ws", "popup")
	if err != nil || len(warnings) != 0 || len(found) != 1 {
		t.Fatalf("nested discovery = %+v, %v, %v", found, warnings, err)
	}
	c := found[0]
	if c.Path != root || len(c.Links) != 3 {
		t.Fatalf("checkout = %+v", c)
	}
	for _, l := range c.Links {
		if l.PaneID == "popup" || l.Current != (l.PaneID == "origin") || l.Path != root || l.LocalBranch != "local" {
			t.Errorf("incorrect link: %+v", l)
		}
		if l.PaneID != "" && l.Label != "topic tab" {
			t.Errorf("missing tab label: %+v", l)
		}
	}
	preferred, ok := (PullRequest{Links: c.Links}).PreferredLink()
	if !ok || preferred.PaneID != "origin" {
		t.Fatalf("current origin not preferred: %+v", preferred)
	}
}

func TestDiscoverWorkspaceFallbackDetachedAndUnusableCheckouts(t *testing.T) {
	root := herdrGitRepo(t)
	herdrGit(t, root, "remote", "add", "origin", "https://github.com/o/r.git")
	for _, origin := range []string{"", "closed-origin"} {
		s := Snapshot{Workspaces: []Workspace{herdrWorkspace("ws", root)}, Panes: []Pane{{ID: "popup", WorkspaceID: "ws", CWD: root}}}
		found, warnings, err := (Discoverer{}).Discover(context.Background(), s, origin, "ws", "popup")
		if err != nil || len(warnings) != 0 || len(found) != 1 || len(found[0].Links) != 1 || found[0].Links[0].PaneID != "" || !found[0].Links[0].Current {
			t.Fatalf("workspace fallback = %+v, %v, %v", found, warnings, err)
		}
	}
	herdrGit(t, root, "checkout", "--detach", "HEAD")
	found, warnings, err := (Discoverer{}).Discover(context.Background(), Snapshot{Panes: []Pane{{ID: "p", CWD: root}}}, "p", "", "")
	if err != nil || len(found) != 1 || found[0].HeadRepository != "" || len(found[0].Repositories) != 0 || found[0].Links[0].Path != root || found[0].Links[0].LocalBranch != "" || !catalogWarning(warnings, "detached HEAD") {
		t.Fatalf("detached = %+v, %v, %v", found, warnings, err)
	}
	other := herdrGitRepo(t)
	herdrGit(t, other, "remote", "add", "origin", "https://gitlab.com/o/r.git")
	found, warnings, err = (Discoverer{}).Discover(context.Background(), Snapshot{Panes: []Pane{{ID: "p", CWD: other}}}, "p", "", "")
	if err != nil || len(found) != 1 || len(warnings) != 0 || found[0].Path != other || found[0].HeadRepository != "" || len(found[0].Repositories) != 0 || found[0].Links[0].LocalBranch != "local" {
		t.Fatalf("non-GitHub = %+v, %v, %v", found, warnings, err)
	}
}

func TestDiscoverWarningsAndCancellation(t *testing.T) {
	s := Snapshot{Panes: []Pane{{ID: "a", CWD: "/offline"}, {ID: "b", CWD: "/offline"}}}
	var calls int
	d := Discoverer{Runner: func(context.Context, ...string) (string, error) { calls++; return "", errors.New("permission denied") }}
	found, warnings, err := d.Discover(context.Background(), s, "a", "", "")
	if err != nil || len(found) != 0 || len(warnings) != 1 || calls != 1 || !catalogWarning(warnings, "permission denied") {
		t.Fatalf("warnings = %+v, %v, %v; calls %d", found, warnings, err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := d.Discover(ctx, s, "", "", ""); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("pre-cancel = %v, calls %d", err, calls)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	d.Runner = func(context.Context, ...string) (string, error) { cancel(); return "", context.Canceled }
	if _, _, err := d.Discover(ctx, s, "", "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("runner cancellation = %v", err)
	}
}

func TestHerdrJumpValidatesExplicitTargetBeforeExactFocus(t *testing.T) {
	root := herdrGitRepo(t)
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	other := herdrGitRepo(t)
	for _, tt := range []struct {
		name, method string
		params       map[string]any
		mutate       func(*Snapshot, *Link)
		wantError    string
	}{
		{name: "pane nested renamed branch", method: "pane.focus", params: map[string]any{"pane_id": "pane"}},
		{name: "agent", method: "agent.focus", params: map[string]any{"target": "pane"}, mutate: func(s *Snapshot, l *Link) { l.Agent = true; s.Panes[0].Agent = "running" }},
		{name: "workspace", method: "workspace.focus", params: map[string]any{"workspace_id": "ws"}, mutate: func(_ *Snapshot, l *Link) { l.PaneID = ""; l.TabID = "" }},
		{name: "tab", method: "tab.focus", params: map[string]any{"tab_id": "tab"}, mutate: func(_ *Snapshot, l *Link) { l.PaneID = "" }},
		{name: "missing workspace", mutate: func(s *Snapshot, _ *Link) { s.Workspaces = nil }, wantError: "workspace closed"},
		{name: "missing pane", mutate: func(s *Snapshot, _ *Link) { s.Panes = nil }, wantError: "pane moved or closed"},
		{name: "pane moved workspace", mutate: func(s *Snapshot, _ *Link) { s.Panes[0].WorkspaceID = "other" }, wantError: "pane moved or closed"},
		{name: "pane moved tab", mutate: func(s *Snapshot, _ *Link) { s.Panes[0].TabID = "other" }, wantError: "pane moved or closed"},
		{name: "agent disappeared", mutate: func(_ *Snapshot, l *Link) { l.Agent = true }, wantError: "agent is no longer running"},
		{name: "wrong checkout root", mutate: func(s *Snapshot, _ *Link) { s.Panes[0].ForegroundCWD = other }, wantError: "changed checkout"},
		{name: "nested branch switched", mutate: func(_ *Snapshot, l *Link) { l.LocalBranch = "previous-local-branch" }, wantError: "changed branch"},
		{name: "pane has no cwd", mutate: func(s *Snapshot, _ *Link) { s.Panes[0].ForegroundCWD = ""; s.Panes[0].CWD = "" }, wantError: "has no checkout"},
		{name: "workspace checkout disappeared", mutate: func(s *Snapshot, l *Link) { l.PaneID = ""; l.TabID = ""; s.Workspaces[0].Worktree = nil }, wantError: "workspace checkout changed"},
		{name: "workspace wrong root", mutate: func(s *Snapshot, l *Link) { l.PaneID = ""; l.TabID = ""; s.Workspaces[0] = herdrWorkspace("ws", other) }, wantError: "changed checkout"},
		{name: "missing tab", mutate: func(s *Snapshot, l *Link) { l.PaneID = ""; s.Tabs = nil }, wantError: "tab closed"},
		{name: "tab moved workspace", mutate: func(s *Snapshot, l *Link) { l.PaneID = ""; s.Tabs[0].WorkspaceID = "other" }, wantError: "tab closed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := Snapshot{FocusedPaneID: "unrelated-focused-pane", Panes: []Pane{{ID: "pane", WorkspaceID: "ws", TabID: "tab", CWD: other, ForegroundCWD: nested}}, Workspaces: []Workspace{herdrWorkspace("ws", root)}, Tabs: []Tab{{ID: "tab", WorkspaceID: "ws"}}}
			link := Link{WorkspaceID: "ws", TabID: "tab", PaneID: "pane", Path: root, LocalBranch: "local", Label: "remote-topic"}
			if tt.mutate != nil {
				tt.mutate(&snapshot, &link)
			}
			s := herdrSnapshotServer(t, snapshot)
			h := &Herdr{SocketPath: s.path, Binary: "never-execute", Runner: func(ctx context.Context, args ...string) (string, error) {
				if len(args) == 0 || args[0] != "git" {
					t.Error("Herdr CLI forbidden")
					return "", errors.New("forbidden")
				}
				return RunCommand(ctx, args...)
			}}
			err := h.Jump(context.Background(), link)
			calls := s.calls()
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) || len(calls) != 1 {
					t.Fatalf("invalid target focused: error %v, calls %+v", err, calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []herdrRequest{{ID: "herdr-prs", Method: "session.snapshot", Params: map[string]any{}}, {ID: "herdr-prs", Method: tt.method, Params: tt.params}}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("focus calls = %+v, want %+v", calls, want)
			}
		})
	}
}

func TestNewHerdrEnvironmentWithoutExecution(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "/offline/socket")
	for _, binary := range []string{"", filepath.Join(t.TempDir(), "missing")} {
		t.Setenv("HERDR_BIN_PATH", binary)
		if h := NewHerdr(); h.Binary != "herdr" || h.SocketPath != "/offline/socket" || h.Runner == nil {
			t.Fatalf("defaults = %+v", h)
		}
	}
	binary := filepath.Join(t.TempDir(), "offline-herdr")
	if err := os.WriteFile(binary, []byte("not executed"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_BIN_PATH", binary)
	if NewHerdr().Binary != "herdr" {
		t.Fatal("non-executable binary accepted")
	}
	if err := os.Chmod(binary, 0700); err != nil {
		t.Fatal(err)
	}
	if NewHerdr().Binary != binary {
		t.Fatal("executable override ignored")
	}
}
