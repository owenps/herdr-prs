package prs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLocalFirstSeenPersistsAcrossReopenAndRefresh(t *testing.T) {
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
	c := Checkout{Path: t.TempDir(), LocalBranch: "Feature/PRIVATE", DefaultBranch: "develop", Links: []Link{{PaneID: "private-pane"}}}
	a, now := cacheFixture(t, g, c)
	first := now.Format(time.RFC3339)
	load := func(app *CachedApp, force bool) {
		t.Helper()
		updates := cacheLoad(t, app, force)
		if len(updates) == 0 {
			t.Fatal("no updates")
		}
		for _, u := range updates {
			if len(u.Catalog.PRs) != 1 || !u.Catalog.PRs[0].IsLocal() {
				t.Fatal(u)
			}
			clock := u.Catalog.PRs[0].GroupTiming
			if !reflect.DeepEqual(clock, &GroupTiming{Group: GroupInProgress, Since: first, Approximate: true}) {
				t.Fatal(clock)
			}
		}
	}
	load(a, false)
	*now = now.Add(10 * 24 * time.Hour) // Observation age survives snapshot retention.
	b := NewCachedApp(a.App)
	b.CacheDir, b.Now = a.CacheDir, a.Now
	b.ScopeProvider = func(context.Context, GitHub) (string, error) { return "another-account", nil }
	load(b, false)
	load(b, true)
	entries, err := os.ReadDir(filepath.Join(a.CacheDir, localObservationDir))
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	path := filepath.Join(a.CacheDir, localObservationDir, entries[0].Name())
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0700 || info.Mode().Perm() != 0600 || strings.Contains(string(data), c.Path) || strings.Contains(string(data), "PRIVATE") || strings.Contains(string(data), "private-") {
		t.Fatal("private identity or nonprivate observation", string(data))
	}
	var fields map[string]any
	if json.Unmarshal(data, &fields) != nil || len(fields) != 2 || fields["version"] != float64(1) || fields["since"] != first || len(entries[0].Name()) != 69 {
		t.Fatal("not a hashed time-only record", fields, entries[0].Name())
	}
}

func TestLocalFirstSeenPathBranchIdentityAndEligibility(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
	a, now := cacheFixture(t, g)
	c := Checkout{Path: root, LocalBranch: "Task", Links: []Link{{PaneID: "a"}}}
	x := c
	x.Path, x.Links = alias, []Link{{PaneID: "b"}}
	input := []Checkout{c, x}
	observed, bad := a.observeCheckouts(context.Background(), input)
	if input[0].LocalTiming != nil || input[1].LocalTiming != nil {
		t.Fatal("mutated discovery slice", input)
	}
	if bad || !reflect.DeepEqual(observed[0].LocalTiming, observed[1].LocalTiming) {
		t.Fatal(observed, bad)
	}
	first := observed[0].LocalTiming.Since
	*now = now.Add(time.Hour)
	x.LocalID, x.Path = root, filepath.Join(root, "nested")
	observed, bad = a.observeCheckouts(context.Background(), []Checkout{x})
	if bad || observed[0].LocalTiming.Since != first {
		t.Fatal(observed, bad)
	}
	c.LocalBranch = "task" // LOCAL branch identity is case-sensitive.
	observed, bad = a.observeCheckouts(context.Background(), []Checkout{c})
	if bad || observed[0].LocalTiming.Since == first || observed[0].LocalTiming.Since != now.Format(time.RFC3339) {
		t.Fatal(observed, bad)
	}
	c.Path = t.TempDir() // Same branch in another checkout has its own observation.
	*now = now.Add(time.Hour)
	observed, bad = a.observeCheckouts(context.Background(), []Checkout{c})
	if bad || observed[0].LocalTiming.Since != now.Format(time.RFC3339) {
		t.Fatal(observed, bad)
	}
	entries, err := os.ReadDir(filepath.Join(a.CacheDir, localObservationDir))
	if err != nil || len(entries) != 3 {
		t.Fatal(entries, err)
	}
	for _, ineligible := range []Checkout{{Path: root}, {Path: root, LocalBranch: "develop", DefaultBranch: "develop"}, {LocalBranch: "Task"}} {
		observed, bad = a.observeCheckouts(context.Background(), []Checkout{ineligible})
		if bad || observed[0].LocalTiming != nil {
			t.Fatal(observed, bad)
		}
	}
	entries, _ = os.ReadDir(filepath.Join(a.CacheDir, localObservationDir))
	if len(entries) != 3 {
		t.Fatal("ineligible observations written", entries)
	}
}

func TestLocalObservationMalformedFutureOversizeAndSymlinkFailSafe(t *testing.T) {
	for _, kind := range []string{"corrupt", "future", "old schema", "zero", "oversized", "symlink", "nonprivate"} {
		t.Run(kind, func(t *testing.T) {
			g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
			a, now := cacheFixture(t, g)
			c := Checkout{Path: t.TempDir(), LocalBranch: "task"}
			dir := filepath.Join(a.CacheDir, localObservationDir)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, observationKey(c)+".json")
			since := now.Add(-time.Hour)
			if kind == "future" {
				since = now.Add(time.Hour)
			}
			data, _ := json.Marshal(localObservation{Version: 1, Since: since})
			switch kind {
			case "corrupt":
				data = []byte("{broken")
			case "oversized":
				data = []byte(strings.Repeat("x", 1025))
			case "old schema":
				data, _ = json.Marshal(localObservation{Version: 99, Since: now.Add(-time.Hour)})
			case "zero":
				data, _ = json.Marshal(localObservation{Version: 1})
			}
			if kind == "symlink" {
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "nonprivate" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			observed, bad := a.observeCheckouts(context.Background(), []Checkout{c})
			if !bad || observed[0].LocalTiming == nil || observed[0].LocalTiming.Since != "" {
				t.Fatal("invented clock", observed, bad)
			}
			read, err := os.ReadFile(path)
			if err != nil || string(read) != string(data) {
				t.Fatal("rewrote invalid observation", err)
			}
		})
	}
}

func TestLocalObservationCancellationDisabledAndUnavailablePersistence(t *testing.T) {
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
	a, now := cacheFixture(t, g)
	c := Checkout{Path: t.TempDir(), LocalBranch: "task"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	observed, _ := a.observeCheckouts(ctx, []Checkout{c})
	entries, err := os.ReadDir(a.CacheDir)
	if err != nil || len(entries) != 0 || observed[0].LocalTiming != nil {
		t.Fatal(observed, entries, err)
	}
	a.CacheDir = ""
	observed, bad := a.observeCheckouts(context.Background(), []Checkout{c})
	first := observed[0].LocalTiming.Since
	if bad || first != now.Format(time.RFC3339) {
		t.Fatal(observed, bad)
	}
	*now = now.Add(time.Hour)
	observed, bad = a.observeCheckouts(context.Background(), []Checkout{c})
	if bad || observed[0].LocalTiming.Since != first {
		t.Fatal(observed, bad)
	}
	b, _ := cacheFixture(t, g, c)
	// A regular file blocking the namespace is deterministic even as root.
	if err := os.WriteFile(filepath.Join(b.CacheDir, localObservationDir), []byte("read-only"), 0600); err != nil {
		t.Fatal(err)
	}
	updates := cacheLoad(t, b, false)
	if len(updates[0].Catalog.PRs) != 1 || !catalogWarning(updates[0].Catalog.Warnings, "Local first-seen timing persistence unavailable") {
		t.Fatal(updates)
	}
	if strings.Contains(strings.Join(updates[0].Catalog.Warnings, " "), b.CacheDir) {
		t.Fatal("persistence warning leaked a path")
	}
}

func TestLocalObservationConcurrentPublication(t *testing.T) {
	g := &cacheGitHub{catalogGitHub: &catalogGitHub{}}
	a, _ := cacheFixture(t, g)
	c := Checkout{Path: t.TempDir(), LocalBranch: "task"}
	first := a.now().Format(time.RFC3339Nano)
	for _, reopen := range []bool{false, true} {
		var wg sync.WaitGroup
		clocks := make(chan string, 12)
		for i := 0; i < 12; i++ {
			instant := a.now()
			if reopen {
				// Distinct later clocks expose overwrites or independently invented starts.
				instant = instant.Add(time.Hour + time.Duration(i)*time.Second)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				b := NewCachedApp(a.App)
				b.CacheDir, b.Now = a.CacheDir, func() time.Time { return instant }
				observed, bad := b.observeCheckouts(context.Background(), []Checkout{c})
				if bad || observed[0].LocalTiming == nil {
					t.Error("concurrent publication failed", observed, bad)
					return
				}
				clocks <- observed[0].LocalTiming.Since
			}()
		}
		wg.Wait()
		close(clocks)
		for clock := range clocks {
			if clock != first {
				t.Fatal("racing observations moved first-seen", first, clock)
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(a.CacheDir, localObservationDir))
	if err != nil || len(entries) != 1 {
		t.Fatal("publication left extra records", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(a.CacheDir, localObservationDir, entries[0].Name()))
	var record localObservation
	if err != nil || json.Unmarshal(data, &record) != nil || record.Since.Format(time.RFC3339Nano) != first {
		t.Fatal("persisted observation moved", string(data), err)
	}
}

func TestLocalObservationPathBranchTuple(t *testing.T) {
	root := t.TempDir()
	first := Checkout{Path: filepath.Join(root, "checkout"), LocalBranch: "topic"}
	second := Checkout{Path: filepath.Join(root, "checkoutt"), LocalBranch: "opic"}
	// Concatenating path + branch without a separator aliases these identities.
	for _, c := range []Checkout{first, second} {
		if err := os.Mkdir(c.Path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	a, now := cacheFixture(t, &cacheGitHub{catalogGitHub: &catalogGitHub{}})
	start := now.Format(time.RFC3339)
	observed, bad := a.observeCheckouts(context.Background(), []Checkout{first})
	if bad || observed[0].LocalTiming.Since != start {
		t.Fatal(observed, bad)
	}
	*now = now.Add(time.Hour)
	observed, bad = a.observeCheckouts(context.Background(), []Checkout{second})
	if bad || observed[0].LocalTiming.Since != now.Format(time.RFC3339) {
		t.Fatal("path/branch identities collided", observed, bad)
	}
	observed, bad = a.observeCheckouts(context.Background(), []Checkout{first})
	if bad || observed[0].LocalTiming.Since != start {
		t.Fatal("second tuple replaced first", observed, bad)
	}
	entries, err := os.ReadDir(filepath.Join(a.CacheDir, localObservationDir))
	if err != nil || len(entries) != 2 {
		t.Fatal(entries, err)
	}
}
