package prs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

const localObservationDir = "local-observations"

// Account-independent observation history, not a GitHub snapshot: no TTL and
// no local identity in the record. Only current eligible checkouts are visited.
type localObservation struct {
	Version int       `json:"version"`
	Since   time.Time `json:"since"`
}

func observationKey(c Checkout) string {
	path := c.LocalID
	if path == "" {
		path = c.Path
	}
	branch := c.LocalBranch
	if branch == "" {
		branch = c.Branch
	}
	if !c.inProgress() || path == "" || branch == "" {
		return ""
	}
	return cacheHash(canonicalPath(path) + "\x00" + branch)
}

func (a *CachedApp) observeCheckouts(ctx context.Context, checkouts []Checkout) ([]Checkout, bool) {
	checkouts = append([]Checkout(nil), checkouts...)
	unavailable := false
	seen := map[string]*GroupTiming{}
	for i := range checkouts {
		if ctx.Err() != nil {
			break
		}
		key := observationKey(checkouts[i])
		if key == "" {
			continue
		}
		timing, ok := seen[key]
		if !ok {
			since, err := a.firstSeen(ctx, key)
			unavailable = unavailable || (err != nil && ctx.Err() == nil)
			timing = &GroupTiming{Group: GroupInProgress, Approximate: true}
			if !since.IsZero() {
				timing.Since = since.UTC().Format(time.RFC3339Nano)
			}
			seen[key] = timing
		}
		checkouts[i].LocalTiming = timing
	}
	return checkouts, unavailable
}

// Reads are bounded and never follow a record symlink. Invalid/future records
// are unknown, not a license to invent a newer start. Old observations survive
// cacheMaxAge because their age is the information being tracked.
func readObservation(path string, now time.Time) (time.Time, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return time.Time{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024 || info.Mode().Perm() != 0600 {
		return time.Time{}, errors.New("invalid observation")
	}
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil || len(data) > 1024 {
		return time.Time{}, errors.New("invalid observation")
	}
	var record localObservation
	if json.Unmarshal(data, &record) != nil || record.Version != 1 || record.Since.IsZero() || record.Since.After(now) {
		return time.Time{}, errors.New("invalid observation")
	}
	return record.Since, nil
}

func privateObservationDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid observation directory")
	}
	return os.Chmod(path, 0700)
}

func (a *CachedApp) firstSeen(ctx context.Context, key string) (time.Time, error) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	now := a.now().UTC()
	if since, ok := a.localSeen[key]; ok && !since.After(now) {
		return since, nil
	}
	remember := func(since time.Time) {
		if a.localSeen == nil {
			a.localSeen = map[string]time.Time{}
		}
		a.localSeen[key] = since
	}
	if a.CacheDir == "" {
		remember(now)
		return now, nil
	}
	dir := filepath.Join(a.CacheDir, localObservationDir)
	// Do not read through a replaced namespace/root symlink, or from a
	// namespace whose permissions expose observations to other users.
	for _, existing := range []string{a.CacheDir, dir} {
		info, err := os.Lstat(existing)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return time.Time{}, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return time.Time{}, errors.New("invalid observation directory")
		}
		if info.Mode().Perm() != 0700 {
			if ctx.Err() != nil {
				return time.Time{}, ctx.Err()
			}
			if err := os.Chmod(existing, 0700); err != nil {
				return time.Time{}, err
			}
		}
	}
	path := filepath.Join(dir, key+".json")
	if since, err := readObservation(path, now); err == nil {
		remember(since)
		return since, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return time.Time{}, err
	}
	// A persistence failure must not prevent the current local row showing its
	// actual first observation. Keep it stable within this popup as well.
	remember(now)
	for _, dir := range []string{a.CacheDir, dir} {
		if ctx.Err() != nil {
			return time.Time{}, ctx.Err()
		}
		if err := privateObservationDir(dir); err != nil {
			return now, err
		}
	}
	data, err := json.Marshal(localObservation{Version: 1, Since: now})
	if err != nil {
		return now, err
	}
	file, err := os.CreateTemp(dir, ".observation-*")
	if err != nil {
		return now, err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Close()
	} else {
		file.Close()
	}
	if err != nil {
		return now, err
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	// Publish a fully written 0600 record atomically, without replacement.
	// Another popup may have observed this branch first: never move it forward.
	if err := os.Link(file.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			since, err := readObservation(path, now)
			if err != nil {
				delete(a.localSeen, key)
				return time.Time{}, err
			}
			remember(since)
			return since, nil
		}
		return now, err
	}
	return now, nil
}
