package prs

import "time"

// Missing enrichment differs from an attempted lookup with unavailable history.
// Old snapshots remain useful, but receive one normal background enrichment.
func needsGroupTiming(p PullRequest) bool {
	_, err := time.Parse(time.RFC3339, p.CreatedAt)
	return err == nil && (p.GroupTiming == nil || p.GroupTiming.Group != p.Group())
}

func missingGroupTimings(prs []PullRequest) bool {
	for _, p := range prs {
		if needsGroupTiming(p) {
			return true
		}
	}
	return false
}

// githubGroupTiming uses only lifecycle transitions. Feedback, commits and
// updatedAt say nothing about entry into the current coarse workflow group.
func githubGroupTiming(p PullRequest, node map[string]any) *GroupTiming {
	timing := &GroupTiming{Group: p.Group()}
	if raw, present := node["createdAt"]; present && raw != nil {
		if _, valid := raw.(string); !valid {
			return timing
		}
	}
	created, createdErr := time.Parse(time.RFC3339, p.CreatedAt)
	if p.CreatedAt != "" && createdErr != nil {
		return timing
	}
	validSince := func(value string) bool {
		since, err := time.Parse(time.RFC3339, value)
		return err == nil && (createdErr != nil || !since.Before(created))
	}
	if p.State == "MERGED" || p.State == "CLOSED" {
		if validSince(p.CompletedAt) {
			timing.Since = p.CompletedAt
		}
		return timing
	}
	if p.State != "OPEN" {
		return timing
	}
	if _, ok := node["isDraft"].(bool); !ok {
		return timing
	}
	connection := githubObject(node["timelineItems"])
	raw, nodesOK := connection["nodes"].([]any)
	count, countOK := githubInt(connection["totalCount"])
	page := githubObject(connection["pageInfo"])
	previous, previousOK := page["hasPreviousPage"].(bool)
	// totalCount includes unfiltered timeline items, so it only bounds nodes.
	// last:1 is intentionally not paginated: older transitions cannot change
	// the latest entry time, but a missing tail or malformed metadata can.
	if !nodesOK || len(raw) > 1 || !countOK || count < len(raw) || !previousOK || !githubBool(page["hasNextPage"], false) {
		return timing
	}
	if len(raw) == 0 {
		if !previous && createdErr == nil {
			timing.Since = p.CreatedAt
		}
		return timing
	}
	event := githubObject(raw[0])
	switch githubText(event["__typename"]) {
	case "ConvertToDraftEvent":
		if !p.Draft {
			return timing
		}
	case "ReadyForReviewEvent":
		if p.Draft {
			return timing
		}
	case "ReopenedEvent":
		// Reopening enters the group selected by the current draft flag.
	default:
		// Closed/merged contradict OPEN; unexpected/null nodes are not history.
		return timing
	}
	if since := githubText(event["createdAt"]); validSince(since) {
		timing.Since = since
	}
	return timing
}
