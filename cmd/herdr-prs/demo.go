package main

import (
	"context"
	"fmt"
	"time"

	"github.com/owenps/herdr-prs/internal/prs"
)

func demoCatalog(ctx context.Context) (prs.Catalog, error) {
	if ctx.Err() != nil {
		return prs.Catalog{}, ctx.Err()
	}
	linked := prs.Link{WorkspaceID: "demo", TabID: "demo:t1", PaneID: "demo:p1", Agent: true, Label: "Fix login redirect", Current: true}
	data := []prs.PullRequest{
		{Repository: "acme/api", Number: 142, Title: "Fix login redirect", Branch: "fix/login", Unresolved: 2, Links: []prs.Link{linked}},
		{Repository: "acme/web", Number: 87, Title: "Tighten account permissions", Branch: "permissions", Checks: []prs.Check{{Name: "tests", Conclusion: "FAILURE"}}},
		{Repository: "acme/api", Number: 139, Title: "Add export endpoint", Branch: "exports", ReviewDecision: "REVIEW_REQUIRED"},
		{Repository: "acme/web", Number: 84, Title: "Improve loading state", Branch: "loading", MergeState: "CLEAN", Mergeable: "MERGEABLE"},
		{Repository: "acme/api", Number: 131, Title: "Fix pagination cursor", Branch: "pagination", State: "MERGED", Links: []prs.Link{{WorkspaceID: "other", Label: "Pagination"}}},
		{Local: &prs.LocalWork{ID: "demo-work", Path: "/worktrees/settings", Repository: "acme/web"}, Title: "Implement account settings", Branch: "settings", Links: []prs.Link{{WorkspaceID: "demo-work"}}},
		{Repository: "acme/api", Number: 145, Title: "Draft API simplification", Branch: "api-simplification", Draft: true},
		{Repository: "acme/web", Number: 79, Title: "Replace abandoned experiment", Branch: "experiment", State: "CLOSED", Links: []prs.Link{{WorkspaceID: "old-work"}}},
		{Repository: "acme/api", Number: 128, Title: "Add request tracing", Branch: "request-tracing", State: "MERGED", Links: []prs.Link{{WorkspaceID: "tracing", Label: "Request tracing"}}},
		{Repository: "acme/web", Number: 76, Title: "Remember dashboard filters", Branch: "dashboard-filters", State: "MERGED"},
		{Repository: "acme/mobile", Number: 54, Title: "Restore offline session", Branch: "offline-session", State: "MERGED"},
		{Repository: "acme/infra", Number: 38, Title: "Rotate staging credentials", Branch: "staging-credentials", State: "MERGED"},
		{Repository: "acme/cli", Number: 63, Title: "Add shell completions", Branch: "shell-completions", State: "MERGED", Links: []prs.Link{{WorkspaceID: "completions", Label: "Shell completions"}}},
		{Repository: "acme/worker", Number: 91, Title: "Deduplicate scheduled jobs", Branch: "deduplicate-jobs", State: "MERGED"},
		{Repository: "acme/api", Number: 125, Title: "Enforce webhook signatures", Branch: "webhook-signatures", State: "MERGED"},
		{Repository: "acme/web", Number: 73, Title: "Improve keyboard navigation", Branch: "keyboard-navigation", State: "MERGED"},
		{Repository: "acme/mobile", Number: 51, Title: "Fix notification deep links", Branch: "notification-links", State: "MERGED"},
		{Repository: "acme/infra", Number: 35, Title: "Enable database backups", Branch: "database-backups", State: "MERGED"},
		{Repository: "acme/api", Number: 148, Title: "Retry transient webhook failures", Branch: "webhook-retries", Unresolved: 3, Links: []prs.Link{{WorkspaceID: "webhooks", Agent: true, Label: "Webhook retries"}}},
		{Repository: "acme/web", Number: 92, Title: "Add command palette", Branch: "command-palette", Unresolved: 1},
		{Repository: "acme/mobile", Number: 58, Title: "Sync saved searches", Branch: "saved-searches", Checks: []prs.Check{{Name: "integration", Conclusion: "FAILURE"}}},
		{Repository: "acme/worker", Number: 96, Title: "Bound concurrent exports", Branch: "export-concurrency", Checks: []prs.Check{{Name: "tests", Status: "IN_PROGRESS"}}, Links: []prs.Link{{WorkspaceID: "exports", Label: "Export worker"}}},
		{Repository: "acme/infra", Number: 42, Title: "Split staging and production networks", Branch: "network-isolation", MergeState: "DIRTY", Mergeable: "CONFLICTING"},
		{Repository: "acme/cli", Number: 67, Title: "Show deployment progress", Branch: "deploy-progress", MergeState: "CLEAN", Mergeable: "MERGEABLE"},
		{Repository: "acme/api", Number: 151, Title: "Add cursor-based audit log", Branch: "audit-log", ReviewDecision: "REVIEW_REQUIRED"},
		{Repository: "acme/web", Number: 95, Title: "Polish empty search results", Branch: "search-empty-state", MergeState: "CLEAN", Mergeable: "MERGEABLE", Links: []prs.Link{{WorkspaceID: "search", Label: "Search results"}}},
		{Repository: "acme/mobile", Number: 60, Title: "Reduce startup latency", Branch: "startup-latency", Checks: []prs.Check{{Name: "device-tests", Status: "QUEUED"}}},
		{Repository: "acme/worker", Number: 98, Title: "Recover interrupted uploads", Branch: "upload-recovery", ReviewDecision: "CHANGES_REQUESTED"},
		{Repository: "acme/api", Number: 153, Title: "Prototype usage-based limits", Branch: "usage-limits", Draft: true, Links: []prs.Link{{WorkspaceID: "limits", Agent: true, Label: "Usage limits"}}},
		{Repository: "acme/web", Number: 97, Title: "Build team activity feed", Branch: "activity-feed", Draft: true},
		{Repository: "acme/mobile", Number: 62, Title: "Explore passkey sign-in", Branch: "passkeys", Draft: true},
		{Repository: "acme/infra", Number: 44, Title: "Plan regional failover", Branch: "regional-failover", Draft: true},
		{Local: &prs.LocalWork{ID: "demo-billing", Path: "/worktrees/billing", Repository: "acme/web"}, Title: "Redesign billing settings", Branch: "billing-settings", Links: []prs.Link{{WorkspaceID: "billing", Agent: true, Label: "Billing settings"}}},
		{Local: &prs.LocalWork{ID: "demo-tokens", Path: "/worktrees/tokens", Repository: "acme/api"}, Title: "Add scoped access tokens", Branch: "scoped-tokens", Links: []prs.Link{{WorkspaceID: "tokens", Label: "Access tokens"}}},
		{Local: &prs.LocalWork{ID: "demo-logs", Path: "/worktrees/logs", Repository: "acme/cli"}, Title: "Stream deployment logs", Branch: "deployment-logs", Links: []prs.Link{{WorkspaceID: "logs", Label: "Deployment logs"}}},
		{Local: &prs.LocalWork{ID: "demo-queue", Path: "/worktrees/queue", Repository: "acme/worker"}, Title: "Investigate queue backpressure", Branch: "queue-backpressure", Links: []prs.Link{{WorkspaceID: "queue", Agent: true, Label: "Queue backpressure"}}},
		{Repository: "acme/api", Number: 119, Title: "Replace legacy search backend", Branch: "search-backend", State: "CLOSED"},
		{Repository: "acme/web", Number: 68, Title: "Experiment with compact navigation", Branch: "compact-navigation", State: "CLOSED"},
		{Repository: "acme/mobile", Number: 46, Title: "Prototype home screen widgets", Branch: "home-widgets", State: "CLOSED"},
		{Repository: "acme/infra", Number: 31, Title: "Evaluate alternate container registry", Branch: "container-registry", State: "CLOSED"},
	}
	now := time.Now().UTC()
	agentStates := []prs.AgentStatus{prs.AgentDone, prs.AgentBlocked, prs.AgentWorking, prs.AgentIdle, prs.AgentUnknown}
	agent := 0
	for i := range data {
		p := &data[i]
		for j := range p.Links {
			if p.Links[j].Agent {
				p.Links[j].AgentStatus = agentStates[agent%len(agentStates)]
				agent++
			}
		}
		since := now.Add(-time.Duration(i+1) * 11 * time.Minute).Format(time.RFC3339)
		if p.IsLocal() {
			p.GroupTiming = &prs.GroupTiming{Group: p.Group(), Since: since, Approximate: true}
			continue
		}
		p.CreatedAt = now.Add(-time.Duration(i+1)*time.Hour - 7*time.Minute).Format(time.RFC3339)
		if p.State == "" {
			p.State = "OPEN"
		}
		p.GroupTiming = &prs.GroupTiming{Group: p.Group(), Since: since}
		if p.State == "MERGED" || p.State == "CLOSED" {
			p.CompletedAt = since
		}
		p.FeedbackComplete = true
		p.ChecksComplete = true
		p.URL = fmt.Sprintf("https://github.com/%s/pull/%d", p.Repository, p.Number)
	}
	return prs.Catalog{PRs: data}, nil
}

func demoDetail(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
	now := time.Now().UTC()
	d := prs.Detail{PR: p, Body: "## Summary\n\nRead-only preview. No GitHub or Herdr requests are made in demo mode.\n\n- **Fix** the login redirect\n- Test the `returnTo` parameter\n\n> Keep redirects on the same origin.\n\n```go\nreturn redirectURL\n```\n",
		Comments: []prs.Comment{
			{Author: "reviewer", Body: "Thanks! Can we cover the empty-state case?", CreatedAt: now.Add(-20 * time.Minute).Format(time.RFC3339)},
			{Author: "author", Body: "Added empty-state coverage and updated the regression test.", CreatedAt: now.Add(-5 * time.Minute).Format(time.RFC3339)},
		}}
	if p.Unresolved > 0 {
		d.Threads = []prs.Thread{{Path: "src/auth.go", Line: 42, Comments: []prs.Comment{{Author: "reviewer", Body: "Please add a regression test for this redirect."}}}}
	}
	return d, nil
}
