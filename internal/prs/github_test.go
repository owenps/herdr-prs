package prs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type ghTestObject = map[string]any

type ghTestRequest struct {
	Query     string       `json:"query"`
	Variables ghTestObject `json:"variables"`
}

func ghTestConnection(nodes ...any) ghTestObject {
	if nodes == nil {
		nodes = []any{}
	}
	return ghTestObject{"nodes": nodes, "totalCount": len(nodes), "pageInfo": ghTestObject{"hasNextPage": false, "endCursor": nil}}
}
func ghTestCheck() ghTestObject {
	return ghTestObject{"__typename": "CheckRun", "name": "tests", "status": "COMPLETED", "conclusion": "SUCCESS", "detailsUrl": "https://ci.example/tests"}
}
func ghTestNode(number int) ghTestObject {
	return ghTestObject{
		"number": number, "title": "Example PR", "url": fmt.Sprintf("https://github.com/o/r/pull/%d", number),
		"repository": ghTestObject{"nameWithOwner": "o/r"}, "headRepository": ghTestObject{"nameWithOwner": "fork/r"},
		"headRefName": "topic", "headRefOid": "abc", "state": "OPEN", "isDraft": false,
		"reviewDecision": "APPROVED", "mergeStateStatus": "CLEAN", "mergeable": "MERGEABLE", "updatedAt": "updated",
		"body": "Description", "reviewThreads": ghTestConnection(), "reviews": ghTestConnection(), "comments": ghTestConnection(),
		"commits": ghTestObject{"nodes": []any{ghTestObject{"commit": ghTestObject{"oid": "abc", "statusCheckRollup": ghTestObject{"contexts": ghTestConnection(ghTestCheck())}}}}},
	}
}
func ghTestCommit(node ghTestObject) ghTestObject {
	return node["commits"].(ghTestObject)["nodes"].([]any)[0].(ghTestObject)["commit"].(ghTestObject)
}
func ghTestContexts(node ghTestObject, value any) {
	ghTestCommit(node)["statusCheckRollup"] = ghTestObject{"contexts": value}
}
func ghTestThread() ghTestObject {
	return ghTestObject{"id": "thread-1", "isResolved": false, "isOutdated": true, "path": "a.go", "line": nil, "startLine": nil,
		"comments": ghTestConnection(ghTestObject{"author": ghTestObject{"login": "reviewer"}, "body": "Please fix", "url": "discussion", "createdAt": "created"})}
}
func ghTestBranch(nodes ...any) ghTestObject {
	return ghTestObject{"repository": ghTestObject{"pullRequests": ghTestConnection(nodes...)}}
}
func ghTestSearch(nodes []any, more any, cursor any, count int) ghTestObject {
	return ghTestObject{"search": ghTestObject{"nodes": nodes, "issueCount": count, "pageInfo": ghTestObject{"hasNextPage": more, "endCursor": cursor}}}
}
func ghTestDetail(node any) ghTestObject {
	return ghTestObject{"repository": ghTestObject{"pullRequest": node}}
}

// HTTP adapter fixtures use only an offline server and in-memory token.
func ghTestServer(t *testing.T, handler func(http.ResponseWriter, *http.Request, ghTestRequest)) *GitHubClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer offline-secret" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect HTTP method or headers") // Do not print headers/tokens.
		}
		var request ghTestRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error("invalid request JSON")
			w.WriteHeader(400)
			return
		}
		if !strings.HasPrefix(strings.TrimSpace(request.Query), "query") || strings.Contains(request.Query, "mutation") {
			t.Error("request is not read-only")
		}
		handler(w, r, request)
	}))
	t.Cleanup(server.Close)
	client := NewGitHubClient()
	client.endpoint = server.URL
	client.httpClient = server.Client()
	client.tokenSource = func(ctx context.Context) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > githubTimeout {
			t.Error("missing 20-second request deadline")
		}
		return "offline-secret", nil
	}
	return client
}
func ghTestQueued(t *testing.T, data ...any) (*GitHubClient, *[]ghTestRequest) {
	t.Helper()
	requests := []ghTestRequest{}
	client := ghTestServer(t, func(w http.ResponseWriter, r *http.Request, request ghTestRequest) {
		requests = append(requests, request)
		if len(requests) > len(data) {
			t.Error("unexpected request")
			w.WriteHeader(500)
			return
		}
		if err := json.NewEncoder(w).Encode(ghTestObject{"data": data[len(requests)-1]}); err != nil {
			t.Error(err)
		}
	})
	return client, &requests
}
func ghTestPR(t *testing.T, node ghTestObject) (PullRequest, *GitHubClient) {
	t.Helper()
	client, _ := ghTestQueued(t, ghTestBranch(node))
	prs, err := client.ForBranch(context.Background(), "o/r", "topic")
	if err != nil || len(prs) != 1 {
		t.Fatalf("branch result: %v, count %d", err, len(prs))
	}
	return prs[0], client
}
func ghTestWarning(warnings []string, text string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, text) {
			return true
		}
	}
	return false
}

// Verify the serialized request, not query constants: fixtures return all fields
// regardless of the selection. Keep lifecycle enrichment fused into each read.
func ghTestLifecycleSelection(t *testing.T, request ghTestRequest) {
	t.Helper()
	query := strings.Join(strings.Fields(request.Query), " ")
	for _, selection := range []string{
		"mergeStateStatus mergeable createdAt updatedAt mergedAt closedAt",
		"timelineItems(last: 1, itemTypes: [CONVERT_TO_DRAFT_EVENT, READY_FOR_REVIEW_EVENT, REOPENED_EVENT, CLOSED_EVENT, MERGED_EVENT]) { nodes { __typename ... on ConvertToDraftEvent { createdAt } ... on ReadyForReviewEvent { createdAt } ... on ReopenedEvent { createdAt } ... on ClosedEvent { createdAt } ... on MergedEvent { createdAt } } totalCount pageInfo { hasPreviousPage hasNextPage } }",
	} {
		if !strings.Contains(query, selection) {
			t.Errorf("outbound request missing lifecycle selection: %s", selection)
		}
	}
	if strings.Contains(query, "ConvertedFromDraftEvent") || strings.Contains(query, "CONVERTED_FROM_DRAFT_EVENT") {
		t.Error("project event requested as PR lifecycle")
	}
}

func TestGitHubBranchReadOnlyForkIdentityAndCompletion(t *testing.T) {
	open, merged, closed, unrelated, deleted := ghTestNode(1), ghTestNode(2), ghTestNode(3), ghTestNode(4), ghTestNode(5)
	open["createdAt"] = "2025-01-02T03:04:05Z"
	// GitHub's totalCount is unfiltered; no lifecycle transition among 27 items.
	open["timelineItems"] = ghTestObject{"nodes": []any{}, "totalCount": 27, "pageInfo": ghTestObject{"hasPreviousPage": false, "hasNextPage": false}}
	merged["state"], merged["mergedAt"], merged["closedAt"] = "MERGED", "merged-time", "closed-time"
	closed["state"], closed["closedAt"] = "CLOSED", "closed-time"
	unrelated["headRefName"] = "other"
	deleted["headRepository"] = nil
	client, requests := ghTestQueued(t, ghTestBranch(open, merged, closed, unrelated, deleted))
	prs, err := client.ForBranch(context.Background(), "o/r", "topic")
	if err != nil || len(prs) != 4 {
		t.Fatalf("branch: %v count %d", err, len(prs))
	}
	if prs[0].Status() != "ready" || prs[1].Status() != "merged" || prs[2].Status() != "closed" {
		t.Fatal("incorrect statuses")
	}
	if prs[0].HeadRepository != "fork/r" || prs[0].HeadOID != "abc" || prs[0].UpdatedAt != "updated" || prs[3].HeadRepository != "" {
		t.Fatal("lost head identity")
	}
	if prs[1].CompletedAt != "merged-time" || prs[2].CompletedAt != "closed-time" {
		t.Fatal("incorrect completion dates")
	}
	if len(client.Warnings()) != 0 || len(*requests) != 1 {
		t.Fatal("unexpected warning/request")
	}
	if prs[0].CreatedAt != "2025-01-02T03:04:05Z" || prs[0].GroupTiming == nil || prs[0].GroupTiming.Since != prs[0].CreatedAt {
		t.Fatal("creation/lifecycle history not normalized", prs[0])
	}
	ghTestLifecycleSelection(t, (*requests)[0])
	query := (*requests)[0].Query
	for _, text := range []string{"headRefName: $branch", "first: 50", "[OPEN, MERGED, CLOSED]", "UPDATED_AT", "direction: DESC", "headRepository", "reviewDecision", "reviewThreads", "isOutdated", "commits(last: 1)", "contexts(first: 100)", "StatusContext", "CheckRun"} {
		if !strings.Contains(query, text) {
			t.Errorf("query missing %s", text)
		}
	}
	if !reflect.DeepEqual((*requests)[0].Variables, ghTestObject{"owner": "o", "name": "r", "branch": "topic"}) {
		t.Fatal("wrong variables")
	}
}

func TestGitHubVariablesNotInterpolated(t *testing.T) {
	branch := "topic\n\" } mutation { nope } #"
	node := ghTestNode(1)
	node["headRefName"] = branch
	client, requests := ghTestQueued(t, ghTestBranch(node))
	prs, err := client.ForBranch(context.Background(), "o/r", branch)
	if err != nil || len(prs) != 1 {
		t.Fatal("failed branch lookup")
	}
	if (*requests)[0].Variables["branch"] != branch || strings.Contains((*requests)[0].Query, branch) {
		t.Fatal("branch interpolated")
	}
}

func TestGitHubBranchCapAndEmptyResults(t *testing.T) {
	nodes := make([]any, 50)
	for i := range nodes {
		nodes[i] = ghTestNode(i + 1)
	}
	data := ghTestBranch(nodes...)
	connection := data["repository"].(ghTestObject)["pullRequests"].(ghTestObject)
	connection["totalCount"], connection["pageInfo"] = 51, ghTestObject{"hasNextPage": true, "endCursor": "next"}
	client, requests := ghTestQueued(t, data, ghTestBranch())
	prs, err := client.ForBranch(context.Background(), "o/r", "topic")
	if err != nil || len(prs) != 50 || len(*requests) != 1 || !ghTestWarning(client.Warnings(), "50-PR limit") {
		t.Fatal("cap not enforced")
	}
	prs, err = client.ForBranch(context.Background(), "o/r", "topic")
	if err != nil || len(prs) != 0 {
		t.Fatal("empty branch is an error")
	}
}

func TestGitHubMinePaginationAndDeduplication(t *testing.T) {
	closed := ghTestNode(3)
	closed["state"] = "CLOSED"
	node := ghTestNode(1)
	node["createdAt"] = "2025-01-02T03:04:05Z"
	node["timelineItems"] = ghTestObject{"nodes": []any{ghTestObject{"__typename": "ReadyForReviewEvent", "createdAt": "2025-01-03T03:04:05Z"}}, "totalCount": 36, "pageInfo": ghTestObject{"hasPreviousPage": true, "hasNextPage": false}}
	duplicate := ghTestNode(1)
	duplicate["repository"] = ghTestObject{"nameWithOwner": "O/R"}
	client, requests := ghTestQueued(t, ghTestObject{"viewer": ghTestObject{"login": "me"}},
		ghTestSearch([]any{node}, true, "cursor-1", 2),
		ghTestSearch([]any{duplicate, ghTestNode(2), closed}, false, nil, 2))
	var calls atomic.Int32
	client.tokenSource = func(context.Context) (string, error) { calls.Add(1); return "offline-secret", nil }
	prs, err := client.Mine(context.Background())
	if err != nil || len(prs) != 2 || prs[0].Number != 1 || prs[1].Number != 2 {
		t.Fatalf("mine: %v count %d", err, len(prs))
	}
	if len(*requests) != 3 || calls.Load() != 1 || len(client.Warnings()) != 0 {
		t.Fatal("wrong request/auth count")
	}
	if prs[0].CreatedAt != "2025-01-02T03:04:05Z" || prs[0].GroupTiming == nil || prs[0].GroupTiming.Since != "2025-01-03T03:04:05Z" {
		t.Fatal("authored lifecycle history not normalized", prs[0])
	}
	ghTestLifecycleSelection(t, (*requests)[1])
	if (*requests)[1].Variables["search"] != "is:pr is:open author:me" || (*requests)[1].Variables["cursor"] != nil || (*requests)[2].Variables["cursor"] != "cursor-1" {
		t.Fatal("wrong search variables")
	}
}

func TestGitHubMissingLifecycleState(t *testing.T) {
	node := ghTestNode(1)
	delete(node, "state")
	p, client := ghTestPR(t, node)
	if p.State != "UNKNOWN" || p.CompletedAt != "" || p.FeedbackComplete || p.Status() != "unknown" || !ghTestWarning(client.Warnings(), "review feedback") {
		t.Fatal("missing lifecycle inferred readiness/completion", p)
	}
}

func TestGitHubMineSearchLimit(t *testing.T) {
	data := []any{ghTestObject{"viewer": ghTestObject{"login": "me"}}}
	for page := 0; page < 10; page++ {
		nodes := make([]any, 100)
		for i := range nodes {
			nodes[i] = ghTestNode(page*100 + i + 1)
		}
		data = append(data, ghTestSearch(nodes, true, fmt.Sprintf("cursor-%d", page), 1001))
	}
	client, requests := ghTestQueued(t, data...)
	prs, err := client.Mine(context.Background())
	if err != nil || len(prs) != 1000 || len(*requests) != 11 {
		t.Fatalf("search cap: %v count %d calls %d", err, len(prs), len(*requests))
	}
	warnings := client.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "1,000") {
		t.Fatal("cap warning missing/not deduplicated")
	}
	warnings[0] = "modified"
	if client.Warnings()[0] == "modified" {
		t.Fatal("warnings alias internal state")
	}
}

func TestGitHubMineBrokenPagination(t *testing.T) {
	for _, test := range []struct {
		name   string
		nodes  []any
		more   any
		cursor any
	}{
		{"null cursor", []any{ghTestNode(1)}, true, nil},
		{"unknown page", []any{ghTestNode(1)}, nil, "next"},
		{"empty page", []any{}, true, "next"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, requests := ghTestQueued(t, ghTestObject{"viewer": ghTestObject{"login": "me"}}, ghTestSearch(test.nodes, test.more, test.cursor, 2))
			if _, err := client.Mine(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(*requests) != 2 || !ghTestWarning(client.Warnings(), "pagination") {
				t.Fatal("unbounded/incomplete pagination")
			}
		})
	}
	client, requests := ghTestQueued(t, ghTestObject{"viewer": ghTestObject{"login": "me"}},
		ghTestSearch([]any{ghTestNode(1)}, true, "same", 3), ghTestSearch([]any{ghTestNode(2)}, true, "same", 3))
	prs, err := client.Mine(context.Background())
	if err != nil || len(prs) != 2 || len(*requests) != 3 || !ghTestWarning(client.Warnings(), "pagination") {
		t.Fatal("repeated cursor not bounded")
	}
	client, _ = ghTestQueued(t, ghTestObject{"viewer": ghTestObject{"login": "me"}}, ghTestSearch([]any{}, false, nil, 0))
	if prs, err := client.Mine(context.Background()); err != nil || len(prs) != 0 || len(client.Warnings()) != 0 {
		t.Fatal("empty mine is an error")
	}
}

func TestGitHubMissingIdentity(t *testing.T) {
	for _, number := range []any{nil, true, 1.5, 0, -1, "1"} {
		node := ghTestNode(1)
		node["number"] = number
		client, _ := ghTestQueued(t, ghTestBranch(node))
		prs, err := client.ForBranch(context.Background(), "o/r", "topic")
		if err != nil || len(prs) != 0 || !ghTestWarning(client.Warnings(), "identity") {
			t.Fatalf("invalid identity accepted: %v", number)
		}
	}
	node := ghTestNode(1)
	delete(node, "repository")
	client, _ := ghTestQueued(t, ghTestBranch(node))
	if prs, err := client.ForBranch(context.Background(), "o/r", "topic"); err != nil || len(prs) != 0 {
		t.Fatal("repository identity guessed")
	}
}

func TestGitHubFeedbackAndDefaults(t *testing.T) {
	resolved := ghTestThread()
	resolved["isResolved"] = true
	current := ghTestThread()
	current["isOutdated"] = false
	node := ghTestNode(1)
	node["reviewThreads"] = ghTestConnection(ghTestThread(), resolved, current)
	pr, _ := ghTestPR(t, node)
	if pr.Unresolved != 2 || pr.Status() != "feedback" {
		t.Fatal("outdated unresolved thread ignored")
	}
	for _, field := range []string{"reviewDecision", "isDraft", "mergeStateStatus", "mergeable"} {
		t.Run("missing "+field, func(t *testing.T) {
			node := ghTestNode(1)
			delete(node, field)
			pr, _ := ghTestPR(t, node)
			if pr.Status() == "ready" {
				t.Fatal("missing data produced ready")
			}
		})
	}
	for i, value := range []any{nil, ghTestObject{}, ghTestObject{"nodes": nil}, ghTestObject{"nodes": []any{}}, ghTestConnection(nil),
		ghTestObject{"nodes": []any{}, "pageInfo": ghTestObject{"hasNextPage": false}},
		ghTestObject{"nodes": []any{}, "totalCount": nil, "pageInfo": ghTestObject{"hasNextPage": false}},
		ghTestObject{"nodes": []any{}, "totalCount": false, "pageInfo": ghTestObject{"hasNextPage": false}},
		ghTestConnection(ghTestObject{"isResolved": nil}), ghTestConnection(ghTestObject{"isResolved": "true"})} {
		t.Run(fmt.Sprintf("incomplete %d", i), func(t *testing.T) {
			node := ghTestNode(1)
			node["reviewThreads"] = value
			pr, client := ghTestPR(t, node)
			if pr.FeedbackComplete || pr.Status() == "ready" || !ghTestWarning(client.Warnings(), "review feedback") {
				t.Fatal("incomplete feedback accepted")
			}
		})
	}
}

func TestGitHubChecksNormalizationAndStatuses(t *testing.T) {
	node := ghTestNode(1)
	ghTestContexts(node, ghTestConnection(ghTestCheck(), ghTestObject{"__typename": "StatusContext", "context": "legacy", "state": "FAILURE", "targetUrl": "https://ci.example/job"}))
	pr, _ := ghTestPR(t, node)
	if !pr.ChecksComplete || pr.Status() != "checks failing" || len(pr.Checks) != 2 {
		t.Fatal("bad check status")
	}
	if !reflect.DeepEqual(pr.Checks, []Check{
		{Name: "tests", Status: "COMPLETED", Conclusion: "SUCCESS", URL: "https://ci.example/tests"},
		{Name: "legacy", State: "FAILURE", URL: "https://ci.example/job"},
	}) {
		t.Fatal("check run/legacy status fields not normalized", pr.Checks)
	}
	for _, test := range []struct {
		field  string
		value  any
		status string
	}{
		{"isDraft", true, "draft"}, {"mergeable", "CONFLICTING", "conflicts"}, {"mergeStateStatus", "DIRTY", "conflicts"},
		{"reviewDecision", "CHANGES_REQUESTED", "feedback"}, {"reviewDecision", "REVIEW_REQUIRED", "in review"}, {"mergeStateStatus", "BLOCKED", "in review"},
	} {
		node := ghTestNode(1)
		node[test.field] = test.value
		pr, _ := ghTestPR(t, node)
		if pr.Status() != test.status {
			t.Errorf("%s: got %s want %s", test.field, pr.Status(), test.status)
		}
	}
	for _, test := range []struct {
		status     any
		conclusion any
		expected   string
	}{
		{"IN_PROGRESS", nil, "checks running"}, {"COMPLETED", "FAILURE", "checks failing"}, {nil, "SUCCESS", "checks running"},
	} {
		node := ghTestNode(1)
		check := ghTestCheck()
		check["status"], check["conclusion"] = test.status, test.conclusion
		ghTestContexts(node, ghTestConnection(check))
		pr, _ := ghTestPR(t, node)
		if pr.Status() != test.expected {
			t.Errorf("got %s want %s", pr.Status(), test.expected)
		}
	}
}

func TestGitHubNullRollupVersusMissingAndStale(t *testing.T) {
	node := ghTestNode(1)
	node["reviewDecision"] = nil
	ghTestCommit(node)["statusCheckRollup"] = nil
	pr, client := ghTestPR(t, node)
	if len(pr.Checks) != 0 || !pr.ChecksComplete || pr.Status() != "ready" || len(client.Warnings()) != 0 {
		t.Fatal("explicit null rollup/decision is not missing")
	}
	for i, commits := range []any{nil, ghTestObject{}, ghTestObject{"nodes": nil}, ghTestObject{"nodes": []any{}}, ghTestObject{"nodes": []any{nil}},
		ghTestObject{"nodes": []any{ghTestObject{"commit": nil}}},
		ghTestObject{"nodes": []any{ghTestObject{"commit": ghTestObject{"oid": "abc"}}}},
		ghTestObject{"nodes": []any{ghTestObject{"commit": ghTestObject{"oid": "stale", "statusCheckRollup": nil}}}},
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			node := ghTestNode(1)
			node["commits"] = commits
			pr, client := ghTestPR(t, node)
			if pr.ChecksComplete || len(pr.Checks) != 0 || pr.Status() == "ready" || !ghTestWarning(client.Warnings(), "checks incomplete") {
				t.Fatal("missing/stale checks accepted")
			}
		})
	}
	node = ghTestNode(1)
	ghTestCommit(node)["oid"] = "stale"
	pr, _ = ghTestPR(t, node)
	if len(pr.Checks) != 0 || pr.ChecksComplete {
		t.Fatal("stale successful checks retained")
	}
}

func TestGitHubIncompleteCheckContexts(t *testing.T) {
	missingConclusion := ghTestCheck()
	delete(missingConclusion, "conclusion")
	missingName := ghTestCheck()
	delete(missingName, "name")
	for i, contexts := range []any{nil, ghTestObject{}, ghTestObject{"nodes": nil}, ghTestConnection(nil), ghTestObject{"nodes": []any{ghTestCheck()}},
		ghTestObject{"nodes": []any{}, "pageInfo": ghTestObject{"hasNextPage": false}},
		ghTestObject{"nodes": []any{}, "totalCount": nil, "pageInfo": ghTestObject{"hasNextPage": false}},
		ghTestConnection(ghTestObject{}), ghTestConnection(missingConclusion), ghTestConnection(missingName),
		ghTestConnection(ghTestObject{"__typename": "StatusContext", "context": "legacy", "state": nil}),
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			node := ghTestNode(1)
			ghTestContexts(node, contexts)
			pr, client := ghTestPR(t, node)
			if pr.ChecksComplete || pr.Status() == "ready" || !ghTestWarning(client.Warnings(), "checks incomplete") {
				t.Fatal("incomplete check contexts accepted")
			}
		})
	}
}

func TestGitHubNestedCapsPreventReady(t *testing.T) {
	for _, field := range []string{"checks", "threads"} {
		t.Run(field, func(t *testing.T) {
			node := ghTestNode(1)
			nodes := make([]any, 100)
			for i := range nodes {
				if field == "checks" {
					nodes[i] = ghTestCheck()
				} else {
					thread := ghTestThread()
					thread["isResolved"] = true
					nodes[i] = thread
				}
			}
			connection := ghTestConnection(nodes...)
			connection["totalCount"] = 101
			connection["pageInfo"] = ghTestObject{"hasNextPage": true}
			if field == "checks" {
				ghTestContexts(node, connection)
			} else {
				node["reviewThreads"] = connection
			}
			pr, client := ghTestPR(t, node)
			if pr.Status() != "unknown" || len(client.Warnings()) != 1 {
				t.Fatal("nested cap produced false ready")
			}
			if field == "checks" && pr.ChecksComplete || field == "threads" && pr.FeedbackComplete {
				t.Fatal("cap marked complete")
			}
		})
	}
}

func TestGitHubDetailAuthorsThreadsAndLocalLinks(t *testing.T) {
	node := ghTestNode(1)
	node["createdAt"] = "2025-01-02T03:04:05Z"
	node["timelineItems"] = ghTestObject{"nodes": []any{ghTestObject{"__typename": "ReopenedEvent", "createdAt": "2025-01-03T03:04:05Z"}}, "totalCount": 27, "pageInfo": ghTestObject{"hasPreviousPage": true, "hasNextPage": false}}
	resolved := ghTestThread()
	resolved["isResolved"] = true
	node["reviewThreads"] = ghTestConnection(ghTestThread(), resolved)
	node["reviews"] = ghTestConnection(ghTestObject{"author": ghTestObject{"login": "reviewer"}, "body": "Review", "state": "COMMENTED", "submittedAt": "2025-01-04T03:04:05Z", "url": "review-url"})
	node["comments"] = ghTestConnection(ghTestObject{"author": nil, "body": "Conversation", "url": "comment-url", "createdAt": "created"})
	client, requests := ghTestQueued(t, ghTestDetail(node))
	original := PullRequest{Repository: "O/R", Number: 1, Title: "Old", Links: []Link{{WorkspaceID: "ws", PaneID: "pane", Agent: true}}}
	detail, err := client.Detail(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	if original.Title != "Old" || detail.PR.Title != "Example PR" || !reflect.DeepEqual(detail.PR.Links, original.Links) {
		t.Fatal("lost/mutated local associations")
	}
	detail.PR.Links[0].WorkspaceID = "changed"
	if original.Links[0].WorkspaceID != "ws" {
		t.Fatal("local links alias input")
	}
	if detail.PR.CreatedAt != "2025-01-02T03:04:05Z" || detail.PR.GroupTiming == nil || detail.PR.GroupTiming.Since != "2025-01-03T03:04:05Z" {
		t.Fatal("detail lifecycle history not normalized", detail.PR)
	}
	if detail.Body != "Description" || !reflect.DeepEqual(detail.Reviews, []Review{{Author: "reviewer", Body: "Review", State: "COMMENTED", SubmittedAt: "2025-01-04T03:04:05Z", URL: "review-url"}}) || !reflect.DeepEqual(detail.Comments, []Comment{{Body: "Conversation", URL: "comment-url", CreatedAt: "created"}}) {
		t.Fatal("bad body/review/comment normalization")
	}
	if !reflect.DeepEqual(detail.Threads, []Thread{{Path: "a.go", Outdated: true, Comments: []Comment{{Author: "reviewer", Body: "Please fix", URL: "discussion", CreatedAt: "created"}}}}) {
		t.Fatal("bad unresolved thread normalization", detail.Threads)
	}
	if detail.PR.Unresolved != 1 || len(detail.Warnings) != 0 || len(*requests) != 1 || (*requests)[0].Variables["number"] != float64(1) {
		t.Fatal("detail query/result mismatch")
	}
	ghTestLifecycleSelection(t, (*requests)[0])
	query := strings.Join(strings.Fields((*requests)[0].Query), " ")
	for _, text := range []string{
		"reviews(last: 100) { nodes { id author { login } body state submittedAt url } totalCount pageInfo { hasNextPage endCursor } }",
		"comments(last: 100) { nodes { id author { login } body createdAt url } totalCount pageInfo { hasNextPage endCursor } }",
		"reviewThreads(first: 100) { nodes { id isResolved isOutdated path line startLine comments(first: 100) { nodes { id author { login } body createdAt url path line } totalCount pageInfo { hasNextPage endCursor } } }",
	} {
		if !strings.Contains(query, text) {
			t.Errorf("detail query missing %s", text)
		}
	}
}

func TestGitHubDetailTruncationWarningsAndDeduplication(t *testing.T) {
	node := ghTestNode(1)
	thread := ghTestThread()
	comments := thread["comments"].(ghTestObject)
	comments["totalCount"], comments["pageInfo"] = 101, ghTestObject{"hasNextPage": true}
	for field, connection := range map[string]ghTestObject{"reviewThreads": ghTestConnection(thread), "reviews": ghTestConnection(), "comments": ghTestConnection()} {
		connection["totalCount"], connection["pageInfo"] = 101, ghTestObject{"hasNextPage": true}
		node[field] = connection
	}
	client, _ := ghTestQueued(t, ghTestDetail(node), ghTestDetail(node))
	pr := PullRequest{Repository: "o/r", Number: 1}
	detail, err := client.Detail(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if detail.PR.FeedbackComplete {
		t.Fatal("truncated threads complete")
	}
	for _, text := range []string{"review feedback", "reviews incomplete", "comments incomplete", "thread thread-1 comments incomplete"} {
		if !ghTestWarning(detail.Warnings, text) {
			t.Errorf("missing warning: %s", text)
		}
	}
	second, err := client.Detail(context.Background(), pr)
	if err != nil || !reflect.DeepEqual(second.Warnings, detail.Warnings) || !reflect.DeepEqual(client.Warnings(), detail.Warnings) {
		t.Fatal("warnings not deduplicated globally and repeated locally")
	}
}

func TestGitHubDetailMissingData(t *testing.T) {
	node := ghTestNode(1)
	thread := ghTestThread()
	thread["comments"] = nil
	node["body"], node["reviews"], node["comments"], node["reviewThreads"] = nil, nil, nil, ghTestConnection(thread)
	client, _ := ghTestQueued(t, ghTestDetail(node))
	detail, err := client.Detail(context.Background(), PullRequest{Repository: "o/r", Number: 1})
	if err != nil || detail.Body != "" || len(detail.Reviews) != 0 || len(detail.Comments) != 0 || len(detail.Threads) != 1 || len(detail.Threads[0].Comments) != 0 || len(detail.Warnings) != 4 {
		t.Fatalf("missing detail: %v warnings %v", err, detail.Warnings)
	}
}

func TestGitHubValidationBeforeAuthentication(t *testing.T) {
	client := NewGitHubClient()
	client.tokenSource = func(context.Context) (string, error) {
		t.Error("validation performed after auth")
		return "", errors.New("unexpected")
	}
	for _, repository := range []string{"https://github.com/o/r", "gitlab.com/o/r", "o", "o/r/extra", "", "o /r", "o/r\n"} {
		if _, err := client.ForBranch(context.Background(), repository, "topic"); err == nil || !strings.Contains(err.Error(), "owner/name") {
			t.Errorf("accepted repository %s", repository)
		}
	}
	if _, err := client.ForBranch(context.Background(), "o/r", ""); err == nil || !strings.Contains(err.Error(), "nonempty") {
		t.Fatal("accepted empty branch")
	}
	if _, err := client.Detail(context.Background(), PullRequest{Repository: "o/r"}); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatal("accepted nonpositive number")
	}
}

func TestGitHubUnavailableObjectsAndMismatchedIdentity(t *testing.T) {
	for _, test := range []struct {
		name     string
		data     any
		detail   bool
		expected string
	}{
		{"null repository", ghTestObject{"repository": nil}, false, "repository unavailable"},
		{"null connection", ghTestObject{"repository": ghTestObject{"pullRequests": nil}}, false, "repository unavailable"},
		{"null PR", ghTestDetail(nil), true, "PR unavailable"},
		{"wrong number", ghTestDetail(ghTestNode(2)), true, "different PR identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, _ := ghTestQueued(t, test.data)
			var err error
			if test.detail {
				_, err = client.Detail(context.Background(), PullRequest{Repository: "o/r", Number: 1})
			} else {
				_, err = client.ForBranch(context.Background(), "o/r", "topic")
			}
			if err == nil || !strings.Contains(err.Error(), test.expected) {
				t.Fatalf("expected %s: %v", test.expected, err)
			}
		})
	}
	for _, viewer := range []any{nil, ghTestObject{}, ghTestObject{"login": nil}, ghTestObject{"login": "invalid\nquery"}} {
		client, requests := ghTestQueued(t, ghTestObject{"viewer": viewer})
		if _, err := client.Mine(context.Background()); err == nil || !strings.Contains(err.Error(), "authenticated user's login") || len(*requests) != 1 {
			t.Fatal("invalid viewer accepted")
		}
	}
	client, _ := ghTestQueued(t, ghTestObject{"viewer": ghTestObject{"login": "me"}}, ghTestObject{"search": nil})
	if _, err := client.Mine(context.Background()); err == nil || !strings.Contains(err.Error(), "no PR search results") {
		t.Fatal("null search accepted")
	}
}

func TestGitHubResponseErrorsNeverExposeSecrets(t *testing.T) {
	for _, test := range []struct {
		name     string
		code     int
		body     string
		expected string
	}{
		{"unauthorized", 401, "offline-secret", "authentication"},
		{"forbidden", 403, "offline-secret", "permissions"},
		{"rate limit", 429, "offline-secret", "rate limit"},
		{"server failure", 502, "offline-secret", "HTTP 502"},
		{"invalid JSON", 200, "offline-secret", "invalid JSON"},
		{"trailing JSON", 200, `{ "data": {} } {"secret":"offline-secret"}`, "invalid JSON"},
		{"null root", 200, "null", "no data"},
		{"array root", 200, "[]", "no data"},
		{"missing data", 200, "{}", "no data"},
		{"null data", 200, `{"data":null}`, "no data"},
		{"array data", 200, `{"data":[]}`, "no data"},
		{"partial GraphQL error", 200, `{"data":{"viewer":{"login":"me"}},"errors":[{"message":"offline-secret"}]}`, "GraphQL error"},
		{"malformed GraphQL error", 200, `{"data":{},"errors":{"message":"offline-secret"}}`, "GraphQL error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := ghTestServer(t, func(w http.ResponseWriter, r *http.Request, request ghTestRequest) {
				w.WriteHeader(test.code)
				_, _ = io.WriteString(w, test.body)
			})
			_, err := client.Mine(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.expected) || strings.Contains(err.Error(), "offline-secret") || strings.Contains(fmt.Sprint(client.Warnings()), "offline-secret") {
				t.Fatal("unhelpful error or secret disclosed")
			}
		})
	}
}

func TestGitHubResponseSizeBound(t *testing.T) {
	client := ghTestServer(t, func(w http.ResponseWriter, r *http.Request, request ghTestRequest) {
		chunk := strings.Repeat(" ", 64<<10)
		for written := 0; written <= githubMaxResponse; written += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	})
	if _, err := client.Mine(context.Background()); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatal("oversized response accepted")
	}
}

type ghTestRoundTripper func(*http.Request) (*http.Response, error)

func (f ghTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubTransportAndAuthErrorsNeverExposeSecrets(t *testing.T) {
	client := NewGitHubClient()
	client.tokenSource = func(context.Context) (string, error) { return "", errors.New("offline-secret") }
	if _, err := client.Mine(context.Background()); err == nil || !strings.Contains(err.Error(), "gh auth login") || strings.Contains(err.Error(), "offline-secret") {
		t.Fatal("token error disclosed")
	}
	client = NewGitHubClient()
	client.tokenSource = func(context.Context) (string, error) { return "offline-secret", nil }
	client.httpClient = &http.Client{Transport: ghTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > githubTimeout {
			t.Error("request missing timeout")
		}
		return nil, errors.New("offline-secret")
	})}
	if _, err := client.Mine(context.Background()); err == nil || !strings.Contains(err.Error(), "network") || strings.Contains(err.Error(), "offline-secret") {
		t.Fatal("transport error disclosed")
	}
}

func TestGitHubAuthenticationOnceAndEnvironmentPrecedence(t *testing.T) {
	client := NewGitHubClient()
	if client.endpoint != "https://api.github.com/graphql" || client.httpClient.Timeout != 20*time.Second {
		t.Fatal("incorrect production defaults")
	}
	var calls atomic.Int32
	client.tokenSource = func(context.Context) (string, error) { calls.Add(1); return "offline-secret", nil }
	for i := 0; i < 2; i++ {
		if token, err := client.authToken(context.Background()); err != nil || token != "offline-secret" {
			t.Fatal("auth failed")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("authentication not cached")
	}
	t.Setenv("GH_TOKEN", "primary-secret")
	t.Setenv("GITHUB_TOKEN", "secondary-secret")
	if token, err := githubAuthToken(context.Background()); err != nil || token != "primary-secret" {
		t.Fatal("GH_TOKEN not preferred")
	}
	t.Setenv("GH_TOKEN", "")
	if token, err := githubAuthToken(context.Background()); err != nil || token != "secondary-secret" {
		t.Fatal("GITHUB_TOKEN not used")
	}
	for _, bad := range []string{"", "  ", "bad\nheader"} {
		client := NewGitHubClient()
		client.tokenSource = func(context.Context) (string, error) { return bad, nil }
		if _, err := client.authToken(context.Background()); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
}

func TestGitHubLazyCLIAuthFallbackOffline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	// Fake gh validates argv; no real CLI/API/credentials or token file involved.
	script := "#!/bin/sh\n[ \"$#\" -eq 4 ] && [ \"$1 $2 $3 $4\" = \"auth token --hostname github.com\" ] || exit 2\n: > \"$HERDR_TEST_AUTH_CALLED\"\nprintf '%s\\n' \"$HERDR_TEST_TOKEN\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", dir)
	t.Setenv("HERDR_TEST_TOKEN", "cli-secret")
	marker := filepath.Join(dir, "auth-called")
	t.Setenv("HERDR_TEST_AUTH_CALLED", marker)
	client := NewGitHubClient()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("construction resolved credentials", err)
	}
	for i := 0; i < 2; i++ {
		if token, err := client.authToken(context.Background()); err != nil || token != "cli-secret" {
			t.Fatal("CLI fallback failed")
		}
		// If called a second time, this failure must not be observed.
		if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nprintf '%s' \"$HERDR_TEST_TOKEN\" >&2\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("authentication did not execute offline CLI", err)
	}
	if _, err := NewGitHubClient().authToken(context.Background()); err == nil || strings.Contains(err.Error(), "cli-secret") || !strings.Contains(err.Error(), "gh auth login") {
		t.Fatal("CLI stderr disclosed/unhelpful error")
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := NewGitHubClient().authToken(context.Background()); err == nil || !strings.Contains(err.Error(), "install gh") {
		t.Fatal("missing CLI error unhelpful")
	}
}

func TestGitHubCanceledContextBeforeAuth(t *testing.T) {
	client := NewGitHubClient()
	client.tokenSource = func(context.Context) (string, error) { t.Error("auth on canceled request"); return "", nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Mine(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestGitHubCancelHTTPAndBodyRead(t *testing.T) {
	for _, bodyRead := range []bool{false, true} {
		t.Run(fmt.Sprint(bodyRead), func(t *testing.T) {
			started := make(chan struct{})
			client := ghTestServer(t, func(w http.ResponseWriter, r *http.Request, request ghTestRequest) {
				if bodyRead {
					_, _ = io.WriteString(w, `{"data":`)
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := client.Mine(ctx); done <- err }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("request did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected cancellation, got %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation not honored")
			}
		})
	}
}

func TestGitHubDeadlineAndConcurrentAuthCancellation(t *testing.T) {
	client := NewGitHubClient()
	authRelease := make(chan struct{})
	client.tokenSource = func(ctx context.Context) (string, error) {
		select {
		case <-authRelease:
			return "offline-secret", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := client.Mine(ctx)
	close(authRelease)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	if _, err := client.authToken(context.Background()); err != nil {
		t.Fatal("first request cancellation poisoned cached authentication")
	}

	client = NewGitHubClient()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	client.tokenSource = func(context.Context) (string, error) {
		calls.Add(1)
		close(started)
		<-release
		return "offline-secret", nil
	}
	var wg sync.WaitGroup
	defer func() { close(release); wg.Wait() }()
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = client.authToken(context.Background()) }()
	<-started
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.authToken(ctx); !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatal("waiting auth cannot cancel/ran twice")
	}
}

func TestGitHubRedirectDoesNotForwardAuthorization(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1); w.WriteHeader(500) }))
	defer destination.Close()
	client := ghTestServer(t, func(w http.ResponseWriter, r *http.Request, request ghTestRequest) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	})
	// Use production redirect policy while retaining the injected local endpoint.
	client.httpClient = NewGitHubClient().httpClient
	if _, err := client.Mine(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatal("redirect accepted")
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("credentials forwarded to redirect")
	}
}
