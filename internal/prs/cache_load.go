package prs

import (
	"context"
	"errors"
	"fmt"
)

// Load discovers/rebinds local checkouts first, emits cached rows, then services
// up to four branch queries before the potentially slow global authored search.
// Each source has its own timestamp: failures cannot make retained data fresh.
func (a *CachedApp) Load(ctx context.Context, force bool, emit func(CatalogUpdate)) error {
	checkouts, warnings, err := a.App.discover(ctx)
	if err != nil {
		return err
	}
	checkouts, timingUnavailable := a.observeCheckouts(ctx, checkouts)
	if timingUnavailable {
		warnings = append(warnings, "Local first-seen timing persistence unavailable")
	}
	// Cold opens publish account-independent local presence before resolving
	// auth. Forced refresh keeps the existing UI snapshot until scoped data is
	// available, rather than flashing linked PRs into provisional local rows.
	queries := checkoutQueries(checkouts)
	if !force && len(checkouts) > 0 && emit != nil && ctx.Err() == nil {
		emit(CatalogUpdate{Catalog: assembleCatalog(nil, a.now(), checkouts, queries, nil, warnings)})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	github := a.App.GitHubFactory()
	scope, err := a.scope(ctx, github)
	if err != nil {
		if !force && len(checkouts) > 0 && emit != nil && ctx.Err() == nil {
			emit(CatalogUpdate{Catalog: assembleCatalog(nil, a.now(), checkouts, queries, nil, append(warnings, err.Error()))})
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	mineRecord, haveMine := a.readCache(scope, "mine")
	mine := mineRecord.PRs
	mineStale := haveMine && (force || !mineRecord.fresh(a.now()) || missingGroupTimings(mine))
	results := make(map[string]branchResult, len(queries))
	stale := make(map[string]bool, len(queries))
	branchWarnings := make(map[string][]string, len(queries))
	var pending []branchQuery
	available := haveMine || len(checkouts) > 0
	for _, q := range queries {
		record, ok := a.readCache(scope, "branch:"+q.key())
		if ok {
			results[q.key()] = branchResult{prs: record.PRs, observedAt: record.SavedAt}
			branchWarnings[q.key()] = record.Warnings
			available = true
			stale[q.key()] = force || !record.fresh(a.now()) || missingGroupTimings(record.PRs)
		}
		if !ok || stale[q.key()] {
			pending = append(pending, q)
		}
	}
	failed := false
	publish := func() {
		if !available || ctx.Err() != nil || emit == nil {
			return
		}
		cached := mineStale || failed
		allWarnings := append([]string(nil), warnings...)
		allWarnings = append(allWarnings, mineRecord.Warnings...)
		for _, q := range queries {
			cached = cached || stale[q.key()]
			allWarnings = append(allWarnings, branchWarnings[q.key()]...)
		}
		allWarnings = append(allWarnings, github.Warnings()...)
		catalog := assembleCatalog(mine, mineRecord.SavedAt, checkouts, queries, results, allWarnings)
		if ctx.Err() == nil {
			emit(CatalogUpdate{Catalog: catalog, Cached: cached})
		}
	}
	if haveMine || len(results) > 0 {
		publish()
	}
	var failures []error
	persist := func(record cacheRecord) {
		if err := a.writeCache(ctx, scope, record); err != nil && ctx.Err() == nil {
			warnings = append(warnings, "Persistent cache unavailable")
		}
	}
	visitBranches(ctx, github, pending, a.now, func(q branchQuery, result branchResult) {
		if result.err != nil {
			failed = true
			stale[q.key()] = true
			failures = append(failures, result.err)
			warnings = append(warnings, fmt.Sprintf("%s (%s): %v", q.repository, q.branch, result.err))
		} else {
			available = true
			result.prs = remotePRs(result.prs)
			results[q.key()] = result
			stale[q.key()] = false
			branchWarnings[q.key()] = github.Warnings()
			persist(cacheRecord{Key: "branch:" + q.key(), SavedAt: result.observedAt, PRs: result.prs, Warnings: github.Warnings()})
		}
		publish()
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	if !haveMine || mineStale {
		fresh, err := github.Mine(ctx)
		observedAt := a.now()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			failed = true
			failures = append(failures, err)
			warnings = append(warnings, fmt.Sprintf("Authored PRs unavailable: %v", err))
		} else {
			available, mineStale = true, false
			mine = remotePRs(fresh)
			mineRecord = cacheRecord{Key: "mine", SavedAt: observedAt, PRs: mine, Warnings: github.Warnings()}
			persist(mineRecord)
		}
		publish()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.Join(failures...)
}

func detailCacheKey(p PullRequest) string {
	if p.IsLocal() {
		return ""
	}
	return fmt.Sprintf("detail:%s#%d", p.Repository, p.Number)
}

// Detail rebinds links from the current row even for fresh hits. Changed head or
// updatedAt invalidate freshness, but old prose can remain visible during fetch.
func (a *CachedApp) Detail(ctx context.Context, p PullRequest, force bool, emit func(DetailUpdate)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.IsLocal() {
		if emit != nil {
			emit(DetailUpdate{Detail: localDetail(p)})
		}
		return ctx.Err()
	}
	github := a.App.GitHubFactory()
	scope, err := a.scope(ctx, github)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	key := detailCacheKey(p)
	record, ok := a.readCache(scope, key)
	ok = ok && record.Detail != nil && detailCacheKey(record.Detail.PR) == key
	fresh := ok && !force && record.fresh(a.now()) && !needsGroupTiming(record.Detail.PR) && record.Detail.PR.HeadOID == p.HeadOID && record.Detail.PR.UpdatedAt == p.UpdatedAt
	publish := func(detail Detail, cached, fromCache bool) {
		if ctx.Err() != nil || emit == nil {
			return
		}
		detail.PR.Links = append([]Link(nil), p.Links...)
		emit(DetailUpdate{Detail: detail, Cached: cached, FromCache: fromCache})
	}
	if ok {
		publish(*record.Detail, !fresh, true)
		if fresh {
			return ctx.Err()
		}
	}
	detail, err := github.Detail(ctx, p)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if ok {
			retained := *record.Detail
			publish(retained, true, true)
		}
		return err
	}
	stored := detail
	stored.PR.Links = nil
	if err := a.writeCache(ctx, scope, cacheRecord{Key: key, SavedAt: a.now(), Detail: &stored}); err != nil && ctx.Err() == nil {
		detail.Warnings = append(append([]string(nil), detail.Warnings...), "Persistent cache unavailable")
	}
	publish(detail, false, false)
	return ctx.Err()
}
