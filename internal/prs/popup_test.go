package prs

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestPopupCapturesActivePaneAndOmitsUnsupportedTarget(t *testing.T) {
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "inherited-other-pane")
	snapshot := Snapshot{FocusedPaneID: "active", FocusedWorkspaceID: "space", Panes: []Pane{{ID: "active", WorkspaceID: "space"}, {ID: "inherited-other-pane", WorkspaceID: "other"}}}
	server := herdrSnapshotServer(t, snapshot)
	h := &Herdr{SocketPath: server.path, Runner: herdrNoCLI(t)}
	if err := h.OpenPopup(context.Background()); err != nil {
		t.Fatalf("popup = %v", err)
	}
	calls := server.calls()
	if len(calls) != 2 || calls[1].Method != "plugin.pane.open" {
		t.Fatalf("calls %+v", calls)
	}
	expected := map[string]any{
		"plugin_id": "herdr-prs", "entrypoint": "picker", "placement": "popup", "focus": true,
		"env": map[string]any{"HERDR_PRS_ORIGIN_PANE": "active", "HERDR_PRS_ORIGIN_WORKSPACE": "space"},
	}
	if !reflect.DeepEqual(calls[1].Params, expected) {
		t.Fatalf("popup params %+v; want %+v (no target_pane_id)", calls[1].Params, expected)
	}
}

func TestPopupOutsideHerdrFailsBeforeInspectingSession(t *testing.T) {
	t.Setenv("HERDR_ENV", "")
	h := &Herdr{Runner: herdrNoCLI(t)}
	if err := h.OpenPopup(context.Background()); err == nil || !strings.Contains(err.Error(), "inside Herdr") {
		t.Fatalf("outside popup = %v", err)
	}
}
