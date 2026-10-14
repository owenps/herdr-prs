package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/term"
	"github.com/owenps/herdr-prs/internal/prs"
)

func TestDemoCatalogJSON(t *testing.T) {
	// Demo listing must work without a running Herdr session.
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "absent.sock"))
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"--demo", "--list"}, &out); err != nil {
		t.Fatal(err)
	}
	var data struct {
		PRs []struct {
			prs.PullRequest
			Status string    `json:"status"`
			Group  prs.Group `json:"group"`
		} `json:"prs"`
	}
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	groups := map[prs.Group]bool{}
	agents := map[prs.AgentStatus]bool{}
	local, unlinked := false, false
	for _, row := range data.PRs {
		p := row.PullRequest
		if seen[p.Key()] || p.Title == "" || p.Branch == "" {
			t.Fatalf("duplicate or incomplete demo work: %+v", p)
		}
		seen[p.Key()] = true
		if row.Status != p.Status() || row.Group != p.Group() {
			t.Fatalf("JSON lost derived status/group: %+v", row)
		}
		if p.IsLocal() {
			local = true
			if p.Repository != "" || p.Number != 0 || p.URL != "" || len(p.Links) == 0 {
				t.Fatalf("local work needs a navigation target, not a GitHub identity: %+v", p)
			}
		} else {
			if p.Repository == "" || p.Number <= 0 || p.URL == "" {
				t.Fatalf("incomplete PR identity: %+v", p)
			}
			unlinked = unlinked || len(p.Links) == 0
		}
		groups[row.Group] = true
		agents[p.AgentStatus()] = true
	}
	if !local || !unlinked {
		t.Fatal("demo must illustrate local work and PRs without worktrees")
	}
	for _, group := range []prs.Group{prs.GroupDone, prs.GroupInReview, prs.GroupInProgress, prs.GroupCancelled} {
		if !groups[group] {
			t.Fatalf("demo omits workflow group %q", group)
		}
	}
	for _, state := range []prs.AgentStatus{prs.AgentNone, prs.AgentBlocked, prs.AgentDone, prs.AgentWorking, prs.AgentIdle, prs.AgentUnknown} {
		if !agents[state] {
			t.Fatalf("demo omits agent state %q", state)
		}
	}
}

func TestDemoDetailsPreserveSelectedWork(t *testing.T) {
	catalog, err := demoCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range catalog.PRs {
		if p.IsLocal() || p.Unresolved == 0 {
			continue
		}
		detail, err := demoDetail(context.Background(), p)
		if err != nil || detail.PR.Key() != p.Key() || detail.Body == "" || len(detail.Comments) == 0 || len(detail.Threads) == 0 {
			t.Fatalf("demo preview lost selected work or discussion: %+v, %v", detail, err)
		}
		return
	}
	t.Fatal("demo lacks a PR with review discussion")
}

func TestCLIRejectsInvalidModes(t *testing.T) {
	for _, args := range [][]string{{"--open", "--demo"}, {"--open", "--list"}, {"extra"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if err := execute(context.Background(), args, &bytes.Buffer{}); err == nil {
				t.Fatalf("accepted %v", args)
			}
		})
	}
	if err := execute(context.Background(), []string{"--help"}, &bytes.Buffer{}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help = %v", err)
	}
}

func TestInteractivePickerRequiresTerminal(t *testing.T) {
	if term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd()) {
		t.Skip("requires redirected input or output")
	}
	err := execute(context.Background(), []string{"--demo"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") || !strings.Contains(err.Error(), "--list") {
		t.Fatalf("missing actionable terminal guard: %v", err)
	}
}

func TestBrowserRejectsUnsafeURLs(t *testing.T) {
	for _, raw := range []string{"", "http://github.com/o/r/pull/1", "https://github.com.evil/o/r/pull/1", "https://secret@github.com/o/r/pull/1", "https://github.com:444/o/r/pull/1", "file:///tmp/file"} {
		err := openURL(context.Background(), raw)
		if err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Fatalf("url %q = %v", raw, err)
		}
	}
}
