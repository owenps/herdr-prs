package prs

import (
	"reflect"
	"testing"
)

const timingCreated = "2025-01-01T00:00:00Z"
const timingChanged = "2025-02-01T00:00:00Z"

func timingHistory(kind, since string, count int) ghTestObject {
	var nodes []any = []any{}
	if kind != "" {
		nodes = append(nodes, ghTestObject{"__typename": kind, "createdAt": since})
	}
	return ghTestObject{"nodes": nodes, "totalCount": count,
		"pageInfo": ghTestObject{"hasPreviousPage": false, "hasNextPage": false}}
}

func TestWorkflowTimingLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, state, event, want string
		draft, previous          bool
		count                    int
		group                    Group
	}{
		// totalCount is unfiltered: empty lifecycle nodes may have a nonzero count.
		{"created ready without transitions", "OPEN", "", timingCreated, false, false, 27, GroupInReview},
		{"created draft", "OPEN", "", timingCreated, true, false, 0, GroupInProgress},
		{"converted draft", "OPEN", "ConvertToDraftEvent", timingChanged, true, false, 36, GroupInProgress},
		// last:1 with older pages still proves the current entry time.
		{"latest ready transition", "OPEN", "ReadyForReviewEvent", timingChanged, false, true, 100, GroupInReview},
		{"reopened ready", "OPEN", "ReopenedEvent", timingChanged, false, false, 4, GroupInReview},
		{"reopened draft", "OPEN", "ReopenedEvent", timingChanged, true, false, 4, GroupInProgress},
		{"merged", "MERGED", "", timingChanged, false, false, 0, GroupDone},
		{"merged draft flag", "MERGED", "", timingChanged, true, false, 0, GroupDone},
		{"closed", "CLOSED", "", timingChanged, false, false, 0, GroupCancelled},
		{"closed draft flag", "CLOSED", "", timingChanged, true, false, 0, GroupCancelled},
		{"ready contradicts draft", "OPEN", "ReadyForReviewEvent", "", true, false, 2, GroupInProgress},
		{"draft contradicts ready", "OPEN", "ConvertToDraftEvent", "", false, false, 2, GroupInReview},
		{"closed contradicts open", "OPEN", "ClosedEvent", "", false, false, 2, GroupInReview},
		{"merged contradicts open", "OPEN", "MergedEvent", "", true, false, 2, GroupInProgress},
		{"not PR ready event", "OPEN", "ConvertedFromDraftEvent", "", false, false, 1, GroupInReview},
		{"commit not lifecycle", "OPEN", "PullRequestCommit", "", false, false, 1, GroupInReview},
		{"unknown lifecycle", "UNKNOWN", "ReopenedEvent", "", false, false, 1, GroupInReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := ghTestNode(1)
			node["state"], node["isDraft"], node["createdAt"] = tc.state, tc.draft, timingCreated
			history := timingHistory(tc.event, timingChanged, tc.count)
			history["pageInfo"].(ghTestObject)["hasPreviousPage"] = tc.previous
			node["timelineItems"] = history
			node["updatedAt"] = "2025-03-01T00:00:00Z"
			if tc.state == "MERGED" {
				node["mergedAt"], node["closedAt"] = timingChanged, "2025-03-01T00:00:00Z"
				delete(node, "timelineItems") // Terminal state doesn't need timeline.
			} else if tc.state == "CLOSED" {
				node["closedAt"], node["mergedAt"] = timingChanged, "2025-03-01T00:00:00Z"
				node["timelineItems"] = nil
			}
			p, ok := NewGitHubClient().pullRequest(node, nil)
			if !ok || !reflect.DeepEqual(p.GroupTiming, &GroupTiming{Group: tc.group, Since: tc.want}) {
				t.Fatalf("timing = %+v, want %q", p.GroupTiming, tc.want)
			}
			if tc.state == "MERGED" || tc.state == "CLOSED" {
				if p.CompletedAt != timingChanged {
					t.Fatal("incorrect completion timestamp", p.CompletedAt)
				}
			}
		})
	}
}

func TestGitHubGroupTimingUnknownHistory(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(ghTestObject)
	}{
		{"omitted", func(n ghTestObject) { delete(n, "timelineItems") }},
		{"null", func(n ghTestObject) { n["timelineItems"] = nil }},
		{"missing draft", func(n ghTestObject) { delete(n, "isDraft") }},
		{"missing state", func(n ghTestObject) { delete(n, "state") }},
		{"invalid creation", func(n ghTestObject) { n["createdAt"] = "created" }},
		{"nonstring creation", func(n ghTestObject) { n["createdAt"] = 7 }},
		{"no creation on empty history", func(n ghTestObject) { delete(n, "createdAt"); n["timelineItems"] = timingHistory("", "", 0) }},
		{"invalid event date", func(n ghTestObject) { n["timelineItems"] = timingHistory("ReopenedEvent", "updated", 2) }},
		{"event before creation", func(n ghTestObject) { n["timelineItems"] = timingHistory("ReopenedEvent", "2024-01-01T00:00:00Z", 2) }},
		{"event missing date", func(n ghTestObject) {
			delete(n["timelineItems"].(ghTestObject)["nodes"].([]any)[0].(ghTestObject), "createdAt")
		}},
		{"null node", func(n ghTestObject) { n["timelineItems"].(ghTestObject)["nodes"] = []any{nil} }},
		{"missing nodes", func(n ghTestObject) { delete(n["timelineItems"].(ghTestObject), "nodes") }},
		{"null nodes", func(n ghTestObject) { n["timelineItems"].(ghTestObject)["nodes"] = nil }},
		{"malformed nodes", func(n ghTestObject) { n["timelineItems"].(ghTestObject)["nodes"] = ghTestObject{} }},
		{"missing count", func(n ghTestObject) { delete(n["timelineItems"].(ghTestObject), "totalCount") }},
		{"invalid count", func(n ghTestObject) { n["timelineItems"].(ghTestObject)["totalCount"] = "2" }},
		{"negative count", func(n ghTestObject) { n["timelineItems"].(ghTestObject)["totalCount"] = -1 }},
		{"count below node count", func(n ghTestObject) { n["timelineItems"] = timingHistory("ReopenedEvent", timingChanged, 0) }},
		{"missing tail", func(n ghTestObject) {
			h := timingHistory("", "", 27)
			h["pageInfo"].(ghTestObject)["hasPreviousPage"] = true
			n["timelineItems"] = h
		}},
		{"multiple tail nodes", func(n ghTestObject) {
			h := n["timelineItems"].(ghTestObject)
			h["nodes"] = append(h["nodes"].([]any), h["nodes"].([]any)[0])
		}},
		{"missing page", func(n ghTestObject) { delete(n["timelineItems"].(ghTestObject), "pageInfo") }},
		{"missing previous", func(n ghTestObject) {
			delete(n["timelineItems"].(ghTestObject)["pageInfo"].(ghTestObject), "hasPreviousPage")
		}},
		{"malformed previous", func(n ghTestObject) {
			n["timelineItems"].(ghTestObject)["pageInfo"].(ghTestObject)["hasPreviousPage"] = "false"
		}},
		{"missing next", func(n ghTestObject) {
			delete(n["timelineItems"].(ghTestObject)["pageInfo"].(ghTestObject), "hasNextPage")
		}},
		{"malformed next", func(n ghTestObject) {
			n["timelineItems"].(ghTestObject)["pageInfo"].(ghTestObject)["hasNextPage"] = "false"
		}},
		{"not latest", func(n ghTestObject) {
			n["timelineItems"].(ghTestObject)["pageInfo"].(ghTestObject)["hasNextPage"] = true
		}},
		{"merged lacks mergedAt", func(n ghTestObject) { n["state"], n["closedAt"] = "MERGED", timingChanged }},
		{"closed lacks closedAt", func(n ghTestObject) { n["state"], n["mergedAt"] = "CLOSED", timingChanged }},
		{"invalid terminal date", func(n ghTestObject) { n["state"], n["mergedAt"] = "MERGED", "merged" }},
		{"terminal before creation", func(n ghTestObject) { n["state"], n["closedAt"] = "CLOSED", "2024-01-01T00:00:00Z" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := ghTestNode(1)
			n["createdAt"], n["updatedAt"] = timingCreated, timingChanged
			n["timelineItems"] = timingHistory("ReopenedEvent", timingChanged, 2)
			tc.edit(n)
			p, _ := NewGitHubClient().pullRequest(n, nil)
			if p.GroupTiming == nil || p.GroupTiming.Group != p.Group() || p.GroupTiming.Since != "" || p.GroupTiming.Approximate {
				t.Fatalf("invented clock: %+v", p.GroupTiming)
			}
		})
	}
}

func TestGitHubGroupTimingKnownEventWithoutCreationAndEqualCreation(t *testing.T) {
	for _, created := range []string{"", timingChanged} {
		n := ghTestNode(1)
		n["createdAt"] = created
		n["timelineItems"] = timingHistory("ReopenedEvent", timingChanged, 1)
		p, _ := NewGitHubClient().pullRequest(n, nil)
		if p.GroupTiming.Since != timingChanged {
			t.Fatal(p.GroupTiming)
		}
	}
}
