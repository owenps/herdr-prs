package ui

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/owenps/herdr-prs/internal/prs"
)

func TestDurationLabelsUseWholeUnitsAndOmitZeroRemainders(t *testing.T) {
	started := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		elapsed time.Duration
		want    string
	}{
		{-time.Second, "00s"}, {0, "00s"}, {9 * time.Second, "09s"}, {59 * time.Second, "59s"},
		{time.Minute, "1m"}, {61 * time.Second, "1m"}, {119 * time.Second, "1m"},
		{2 * time.Minute, "2m"}, {time.Hour - time.Second, "59m"},
		{time.Hour, "1h"}, {time.Hour + time.Second, "1h"},
		{time.Hour + time.Minute + time.Second, "1h1m"}, {123 * time.Minute, "2h3m"},
		{24*time.Hour - time.Second, "23h59m"}, {24 * time.Hour, "1d"},
		{24*time.Hour + time.Minute + time.Second, "1d"}, {25 * time.Hour, "1d1h"},
		{48*time.Hour - time.Second, "1d23h"}, {48 * time.Hour, "2d"},
	} {
		if got := formatAge(started.Format(time.RFC3339Nano), started.Add(tc.elapsed)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.elapsed, got, tc.want)
		}
	}
	for _, since := range []string{"", "invalid"} {
		if got := formatAge(since, started); got != "" {
			t.Errorf("unavailable timestamp %q displayed %q", since, got)
		}
	}
}

func TestListAgeRequiresTimingForTheCurrentGroup(t *testing.T) {
	now := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
	p := fixture(1)
	p.CreatedAt = now.Add(-72 * time.Hour).Format(time.RFC3339)
	p.UpdatedAt = now.Add(-time.Hour).Format(time.RFC3339)
	for _, timing := range []*prs.GroupTiming{
		nil,
		{Group: p.Group()},
		{Group: p.Group(), Since: "invalid", Approximate: true},
		{Group: prs.GroupDone, Since: p.CreatedAt},
		{Since: p.CreatedAt},
	} {
		p.GroupTiming = timing
		if age := groupAge(p, now); age != "" {
			t.Fatalf("unavailable timing %+v displayed %q", timing, age)
		}
		layout := listRowLayout([]prs.PullRequest{p}, 100, now)
		if layout.age != 0 {
			t.Fatal("unknown clock allocated a creation-age column")
		}
		known := fixture(2)
		known.GroupTiming = &prs.GroupTiming{Group: known.Group(), Since: p.UpdatedAt}
		shared := listRowLayout([]prs.PullRequest{p, known}, 100, now)
		ageStart := 2 + shared.iconWidth + shared.title + 1
		row := ansi.Strip(shared.render(p, now, false))
		if shared.age == 0 || ansi.Cut(row, ageStart, ageStart+shared.age) != strings.Repeat(" ", shared.age) {
			t.Fatal("unknown clock showed fallback age in shared column", row)
		}
	}
}

func TestClockUpdatesListResidenceAndDetailCreationWithoutFetchingOrParsing(t *testing.T) {
	created := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	p := fixture(1)
	p.CreatedAt = created.Format(time.RFC3339)
	p.GroupTiming = &prs.GroupTiming{Group: p.Group(), Since: created.Add(30 * time.Second).Format(time.RFC3339)}
	local := localFixture("local", "/work/local")
	local.Links = nil // Keep the PR selected so its creation header is visible.
	local.CreatedAt = created.Add(-72 * time.Hour).Format(time.RFC3339)
	local.GroupTiming = &prs.GroupTiming{Group: local.Group(), Since: p.CreatedAt, Approximate: true}
	loads, details := 0, 0
	m := modelWith(t, Actions{
		Load: func(context.Context) (prs.Catalog, error) {
			loads++
			return prs.Catalog{PRs: []prs.PullRequest{p, local}}, nil
		},
		Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
			details++
			return prs.Detail{PR: p, Body: "# Clock-independent prose"}, nil
		},
	})
	m.Update(clockMsg(created.Add(58 * time.Second)))
	m.Update(tea.WindowSizeMsg{Width: 180, Height: 24})
	_, render := m.Update(previewRequest(t, m)())
	finishRender(t, m, render)
	epoch := m.renderEpoch // Supplement visible assertions with a parser-work guard.
	for i, want := range []string{"59s", "1m"} {
		_, next := m.Update(clockMsg(created.Add(time.Duration(59+i) * time.Second)))
		if next == nil {
			t.Fatal("clock stopped ticking")
		}
		left, _ := m.paneWidths()
		var list, detail strings.Builder
		for _, line := range strings.Split(ansi.Strip(m.View()), "\n") {
			list.WriteString(ansi.Cut(line, 0, left) + "\n")
			detail.WriteString(ansi.Cut(line, left+1, m.width) + "\n")
		}
		groupWant := []string{"29s", "30s"}[i]
		if !strings.Contains(list.String(), groupWant) || !strings.Contains(list.String(), want) || strings.Contains(list.String(), "~") || strings.Contains(list.String(), "3d") {
			t.Fatalf("PR/local residence clocks did not advance independently: %q", list.String())
		}
		if !strings.Contains(detail.String(), "created "+want) || !strings.Contains(detail.String(), "Clock-independent prose") {
			t.Fatalf("creation header or cached prose lost: %q", detail.String())
		}
		if _, ok := next().(clockMsg); !ok || loads != 1 || details != 1 || m.renderEpoch != epoch {
			t.Fatal("age tick fetched data or scheduled Markdown instead of the next clock")
		}
	}
	// Approximation is provenance, not a printed prefix or a creation clock.
	for _, row := range m.rows {
		if row.IsLocal() && (row.GroupTiming == nil || !row.GroupTiming.Approximate) {
			t.Fatal("local approximation provenance was lost")
		}
	}
}

func TestGroupTimingSnapshotsDoNotAliasInputs(t *testing.T) {
	p := fixture(1)
	p.GroupTiming = &prs.GroupTiming{Group: p.Group(), Since: "2026-01-01T00:00:00Z", Approximate: true}
	d := prs.Detail{PR: p, Body: "prose"}
	cloned := cloneRenderDetail(d)
	catalog := cloneCatalog(prs.Catalog{PRs: []prs.PullRequest{p}})
	if !reflect.DeepEqual(cloned, d) || cloned.PR.GroupTiming == p.GroupTiming || catalog.PRs[0].GroupTiming == p.GroupTiming {
		t.Fatal("clock pointer not deep-cloned")
	}
	p.GroupTiming.Group, p.GroupTiming.Since, p.GroupTiming.Approximate = prs.GroupDone, "changed", false
	for _, timing := range []*prs.GroupTiming{cloned.PR.GroupTiming, catalog.PRs[0].GroupTiming} {
		if timing.Group != prs.GroupInReview || timing.Since != "2026-01-01T00:00:00Z" || !timing.Approximate {
			t.Fatal("input mutation corrupted snapshot", timing)
		}
	}
}

func TestCatalogClockRebindAndCachedProseNeverRegressTiming(t *testing.T) {
	m, d := renderedPreview(t)
	m.Update(clockMsg(time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)))
	frame, epoch := slices.Clone(m.detailLines()), m.renderEpoch
	m.detailTop = 5
	fresh := cloneCatalog(m.catalog)
	fresh.PRs[0].GroupTiming = &prs.GroupTiming{Group: fresh.PRs[0].Group(), Since: m.clock.Add(-123 * time.Minute).Format(time.RFC3339)}
	if _, cmd := m.Update(catalogUpdateMsg{token: m.loadToken, update: prs.CatalogUpdate{Catalog: fresh}}); cmd != nil {
		t.Fatal("clock-only catalog update scheduled work")
	}
	d.PR.GroupTiming = &prs.GroupTiming{Group: d.PR.Group(), Since: "2025-01-01T00:00:00Z", Approximate: true}
	for _, flags := range []prs.DetailUpdate{{Cached: true}, {FromCache: true}} {
		flags.Detail = d
		if _, cmd := m.Update(detailUpdateMsg{token: m.detailToken, key: d.PR.Key(), update: flags}); cmd != nil || !strings.Contains(plain(m), "2h3m") {
			t.Fatal("cached prose regressed the visible catalog clock or scheduled rendering")
		}
	}
	if !slices.Equal(frame, m.detailLines()) || !strings.Contains(plain(m), "Old styled heading") || m.detailTop != 5 || m.renderEpoch != epoch {
		t.Fatal("clock rebinding disturbed rendered prose or scroll")
	}
	press(m, tea.KeyDown)
	if cmd := press(m, tea.KeyUp); cmd != nil || !strings.Contains(plain(m), "2h3m") {
		t.Fatal("in-memory detail cache overrode fresh catalog clock")
	}
	// A live response with unavailable history clears the old clock, even
	// without a group transition. Never retain plausible but stale history.
	d.PR.GroupTiming = nil
	if _, cmd := m.Update(detailUpdateMsg{token: m.detailToken, key: d.PR.Key(), update: prs.DetailUpdate{Detail: d}}); cmd != nil {
		t.Fatal("live unknown clock reparsed prose")
	}
	if strings.Contains(plain(m), "2h3m") || m.detail.PR.GroupTiming != nil {
		t.Fatal("live unknown history retained stale clock")
	}
}

func TestDetailCheckAndProseChangesReachTheRenderedView(t *testing.T) {
	p := fixture(1)
	m := wideModel(t, Actions{Detail: func(_ context.Context, p prs.PullRequest) (prs.Detail, error) {
		return prs.Detail{PR: p, Body: "Original prose"}, nil
	}}, p)
	m.Update(tea.WindowSizeMsg{Width: 180, Height: 30})
	_, render := m.Update(previewRequest(t, m)())
	finishRender(t, m, render)
	for _, tc := range []struct {
		change func(*prs.Detail)
		want   string
	}{
		{func(d *prs.Detail) { d.PR.Checks = []prs.Check{{Name: "New check", Conclusion: "SUCCESS"}} }, "New check · SUCCESS"},
		{func(d *prs.Detail) { d.Body = "Replacement prose" }, "Replacement prose"},
	} {
		changed := cloneRenderDetail(*m.detail)
		tc.change(&changed)
		_, render := m.Update(detailUpdateMsg{token: m.detailToken, key: p.Key(), update: prs.DetailUpdate{Detail: changed}})
		finishRender(t, m, render)
		if !strings.Contains(plain(m), tc.want) {
			t.Fatalf("content update hidden by render cache: want %q in %q", tc.want, plain(m))
		}
	}
	if strings.Contains(plain(m), "Original prose") {
		t.Fatal("replaced body retained stale prose")
	}
}
