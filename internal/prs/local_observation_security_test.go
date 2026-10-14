package prs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalObservationRejectsSymlinkedNamespace(t *testing.T) {
	for _, namespace := range []string{"root", "observations"} {
		t.Run(namespace, func(t *testing.T) {
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			c := Checkout{Path: t.TempDir(), LocalBranch: "feature"}
			a := NewCachedApp(&App{})
			a.CacheDir, a.Now = t.TempDir(), func() time.Time { return now }
			target := t.TempDir()
			recordDir := target
			if namespace == "root" {
				recordDir = filepath.Join(target, localObservationDir)
				if err := os.Mkdir(recordDir, 0700); err != nil {
					t.Fatal(err)
				}
				a.CacheDir = filepath.Join(a.CacheDir, "linked-root")
			}
			data, err := json.Marshal(localObservation{Version: 1, Since: now.Add(-time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(recordDir, observationKey(c)+".json")
			if err := os.WriteFile(record, data, 0600); err != nil {
				t.Fatal(err)
			}
			link := a.CacheDir
			if namespace == "observations" {
				link = filepath.Join(a.CacheDir, localObservationDir)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			observed, bad := a.observeCheckouts(context.Background(), []Checkout{c})
			if !bad || observed[0].LocalTiming == nil || observed[0].LocalTiming.Since != "" {
				t.Fatal("trusted clock through symlink", observed, bad)
			}
			got, err := os.ReadFile(record)
			if err != nil || string(got) != string(data) {
				t.Fatal("modified external observation", err)
			}
			entries, err := os.ReadDir(recordDir)
			if err != nil || len(entries) != 1 {
				t.Fatal("wrote through symlink", entries, err)
			}
		})
	}
}
