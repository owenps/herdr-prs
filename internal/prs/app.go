package prs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Session interface {
	Snapshot(context.Context) (Snapshot, error)
}
type App struct {
	Herdr                                  Session
	GitHubFactory                          func() GitHub
	Discover                               func(context.Context, Snapshot, string, string, string) ([]Checkout, []string, error)
	OriginPane, OriginWorkspace, PopupPane string
}

func NewApp(h *Herdr) *App {
	origin, opened := os.LookupEnv("HERDR_PRS_ORIGIN_PANE")
	if !opened {
		origin = os.Getenv("HERDR_PANE_ID")
	}
	workspace := os.Getenv("HERDR_PRS_ORIGIN_WORKSPACE")
	if !opened {
		workspace = os.Getenv("HERDR_WORKSPACE_ID")
	}
	popup := ""
	if opened {
		popup = os.Getenv("HERDR_PANE_ID")
	}
	return &App{Herdr: h, GitHubFactory: func() GitHub { return NewGitHubClient() }, Discover: (Discoverer{Runner: RunCommand}).Discover,
		OriginPane: origin, OriginWorkspace: workspace, PopupPane: popup}
}

func matches(p PullRequest, c Checkout) bool {
	return !p.IsLocal() && p.Branch == c.Branch && p.HeadRepository != "" && strings.EqualFold(p.HeadRepository, c.HeadRepository)
}

// Completed history belongs to feature checkouts, not confirmed defaults.
// Keep raw matches unchanged for navigation and other head-identity checks.
func catalogMatches(p PullRequest, c Checkout) bool {
	return matches(p, c) && !((p.State == "MERGED" || p.State == "CLOSED") && c.onDefaultBranch())
}

func (c Checkout) onDefaultBranch() bool {
	branch := c.LocalBranch
	if branch == "" {
		branch = c.Branch
	}
	return c.DefaultBranch != "" && branch == c.DefaultBranch
}

type branchQuery struct{ repository, branch string }

func (q branchQuery) key() string { return strings.ToLower(q.repository) + "\x00" + q.branch }

type branchResult struct {
	prs        []PullRequest
	err        error
	observedAt time.Time
}

func branchLookups(ctx context.Context, github GitHub, queries []branchQuery) map[string]branchResult {
	results := make(map[string]branchResult, len(queries))
	visitBranches(ctx, github, queries, time.Now, func(q branchQuery, r branchResult) { results[q.key()] = r })
	return results
}

// visit runs on the caller, never concurrently. Workers cannot emit late data.
func visitBranches(ctx context.Context, github GitHub, queries []branchQuery, now func() time.Time, visit func(branchQuery, branchResult)) {
	type response struct {
		query  branchQuery
		result branchResult
	}
	jobs := make(chan branchQuery, len(queries))
	for _, q := range queries {
		jobs <- q
	}
	close(jobs)
	responses := make(chan response, len(queries))
	var workers sync.WaitGroup
	for i := 0; i < min(4, len(queries)); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for q := range jobs {
				if ctx.Err() != nil {
					return
				}
				found, err := github.ForBranch(ctx, q.repository, q.branch)
				responses <- response{q, branchResult{prs: found, err: err, observedAt: now()}}
			}
		}()
	}
	go func() { workers.Wait(); close(responses) }()
	for {
		select {
		case <-ctx.Done():
			return
		case r, ok := <-responses:
			if !ok {
				return
			}
			if ctx.Err() == nil {
				visit(r.query, r.result)
			}
		}
	}
}

// discover always runs before GitHub work, including on cache hits.
func (a *App) discover(ctx context.Context) ([]Checkout, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var checkouts []Checkout
	var warnings []string
	snapshot, err := a.Herdr.Snapshot(ctx)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("Herdr linkage unavailable: %v", err))
	} else {
		var discoveryWarnings []string
		checkouts, discoveryWarnings, err = a.Discover(ctx, snapshot, a.OriginPane, a.OriginWorkspace, a.PopupPane)
		if err != nil {
			return nil, nil, err
		}
		warnings = append(warnings, discoveryWarnings...)
	}
	return checkouts, warnings, ctx.Err()
}

func checkoutQueries(checkouts []Checkout) []branchQuery {
	var queries []branchQuery
	seen := map[string]bool{}
	for _, checkout := range checkouts {
		if checkout.HeadRepository == "" || checkout.Branch == "" {
			continue
		}
		for _, repo := range checkout.Repositories {
			q := branchQuery{repo, checkout.Branch}
			if !seen[q.key()] {
				seen[q.key()] = true
				queries = append(queries, q)
			}
		}
	}
	return queries
}

// assembleCatalog never trusts remote or persisted links. Branch-only PRs are
// retained exclusively by current checkout identity, never as authored rows.
func assembleCatalog(mine []PullRequest, mineObservedAt time.Time, checkouts []Checkout, queries []branchQuery, results map[string]branchResult, warnings []string) Catalog {
	authored := map[string]PullRequest{}
	candidates := map[string]PullRequest{}
	observations := map[string]time.Time{}
	for _, p := range remotePRs(mine) {
		p.Links = nil
		candidates[p.Key()] = p
		observations[p.Key()] = mineObservedAt
	}
	for _, q := range queries {
		result := results[q.key()]
		if result.err != nil {
			warnings = append(warnings, fmt.Sprintf("%s (%s): %v", q.repository, q.branch, result.err))
			continue
		}
		for _, p := range remotePRs(result.prs) {
			old, exists := candidates[p.Key()]
			// CI can change without advancing updatedAt. For the same PR
			// revision, prefer the most recently fetched source, not branch order.
			if !exists || p.UpdatedAt > old.UpdatedAt || p.UpdatedAt == old.UpdatedAt && !result.observedAt.Before(observations[p.Key()]) {
				p.Links = nil
				candidates[p.Key()] = p
				observations[p.Key()] = result.observedAt
			}
		}
	}
	for _, p := range remotePRs(mine) {
		if current := candidates[p.Key()]; current.State != "MERGED" && current.State != "CLOSED" {
			authored[p.Key()] = current
		}
	}
	for _, checkout := range checkouts {
		var opened, completed []PullRequest
		for _, p := range candidates {
			if !catalogMatches(p, checkout) {
				continue
			}
			if p.State == "OPEN" {
				opened = append(opened, p)
			} else if p.State == "MERGED" || p.State == "CLOSED" {
				completed = append(completed, p)
			}
		}
		if len(opened) == 0 && len(completed) > 0 {
			sort.Slice(completed, func(i, j int) bool {
				timestamp := func(p PullRequest) string {
					if p.CompletedAt != "" {
						return p.CompletedAt
					}
					return p.UpdatedAt
				}
				x, y := timestamp(completed[i]), timestamp(completed[j])
				if x != y {
					return x > y
				}
				return completed[i].Key() < completed[j].Key()
			})
			opened = completed[:1]
		}
		if len(opened) == 0 && checkout.inProgress() {
			p := localPullRequest(checkout)
			if existing, ok := authored[p.Key()]; ok && existing.IsLocal() {
				for _, link := range p.Links {
					existing.Links = appendLink(existing.Links, link)
				}
				p = existing
			}
			authored[p.Key()] = p
		}
		for _, p := range opened {
			authored[p.Key()] = p
		}
	}
	catalog := Catalog{PRs: make([]PullRequest, 0, len(authored))}
	for _, p := range authored {
		for _, checkout := range checkouts {
			if catalogMatches(p, checkout) {
				for _, link := range checkout.Links {
					p.Links = appendLink(p.Links, link)
				}
			}
		}
		catalog.PRs = append(catalog.PRs, p)
	}
	sort.Slice(catalog.PRs, func(i, j int) bool {
		a, b := catalog.PRs[i], catalog.PRs[j]
		if a.Group() != b.Group() {
			return a.Group().Order() < b.Group().Order()
		}
		if a.Priority() != b.Priority() {
			return a.Priority() < b.Priority()
		}
		return a.Key() < b.Key()
	})
	catalog.Warnings = unique(warnings)
	return catalog
}

// Load is the always-live CLI path; CachedApp adds persistence and staged emits.
func (a *App) Load(ctx context.Context) (Catalog, error) {
	checkouts, warnings, err := a.discover(ctx)
	if err != nil {
		return Catalog{}, err
	}
	github := a.GitHubFactory()
	queries := checkoutQueries(checkouts)
	results := branchLookups(ctx, github, queries)
	if err := ctx.Err(); err != nil {
		return Catalog{}, err
	}
	mine, err := github.Mine(ctx)
	mineObservedAt := time.Now()
	if err != nil {
		return Catalog{}, err
	}
	if err := ctx.Err(); err != nil {
		return Catalog{}, err
	}
	return assembleCatalog(mine, mineObservedAt, checkouts, queries, results, append(warnings, github.Warnings()...)), nil
}

func (a *App) Detail(ctx context.Context, p PullRequest) (Detail, error) {
	if err := ctx.Err(); err != nil {
		return Detail{}, err
	}
	if p.IsLocal() {
		return localDetail(p), nil
	}
	return a.GitHubFactory().Detail(ctx, p)
}

// Local rows represent named branch work, not mere workspace presence. An
// unknown default is not guessed; terminal/detached and non-Git spaces have no
// branch. Matching OPEN PRs remain eligible even on a confirmed default.
func (c Checkout) inProgress() bool {
	branch := c.LocalBranch
	if branch == "" {
		branch = c.Branch
	}
	return branch != "" && branch != c.DefaultBranch
}

func localPullRequest(c Checkout) PullRequest {
	id := c.LocalID
	if id == "" {
		id = canonicalPath(c.Path)
	}
	if id == "" {
		if link, ok := (PullRequest{Links: c.Links}).PreferredLink(); ok {
			id = "workspace:" + link.WorkspaceID
			if link.WorkspaceID == "" {
				id = "pane:" + link.PaneID
			}
		}
	}
	title := c.Title
	if title == "" {
		title = c.LocalBranch
	}
	if title == "" {
		title = c.Branch
	}
	if title == "" && c.Path != "" {
		title = filepath.Base(c.Path)
	}
	if title == "" {
		if link, ok := (PullRequest{Links: c.Links}).PreferredLink(); ok {
			title = link.Label
		}
	}
	if title == "" {
		title = "Local workspace"
	}
	return PullRequest{Local: &LocalWork{ID: id, Path: c.Path, Repository: c.HeadRepository}, GroupTiming: c.LocalTiming, Title: title, Branch: c.LocalBranch, Links: append([]Link(nil), c.Links...)}
}

func localDetail(p PullRequest) Detail {
	body := "No linked pull request."
	if p.Local.Path != "" {
		body += "\n\nPath: " + p.Local.Path
	}
	if p.Branch != "" {
		body += "\nBranch: " + p.Branch
	}
	return Detail{PR: p, Body: body, Reviews: []Review{}, Comments: []Comment{}, Threads: []Thread{}}
}
