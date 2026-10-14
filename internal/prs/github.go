package prs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	githubEndpoint    = "https://api.github.com/graphql"
	githubTimeout     = 20 * time.Second
	githubMaxResponse = 16 << 20
	githubPage        = `pageInfo { hasNextPage endCursor }`
	githubChecks      = `commits(last: 1) {
  nodes { commit { oid statusCheckRollup { contexts(first: 100) {
    nodes {
      __typename
      ... on CheckRun { name status conclusion detailsUrl }
      ... on StatusContext { context state targetUrl }
    }
    totalCount pageInfo { hasNextPage endCursor }
  } } } }
}`
	githubThreadFields = `id isResolved isOutdated path line startLine`
	githubCore         = `id number title url headRefName headRefOid state isDraft
reviewDecision mergeStateStatus mergeable createdAt updatedAt mergedAt closedAt
repository { nameWithOwner } headRepository { nameWithOwner }
timelineItems(last: 1, itemTypes: [CONVERT_TO_DRAFT_EVENT, READY_FOR_REVIEW_EVENT, REOPENED_EVENT, CLOSED_EVENT, MERGED_EVENT]) {
 nodes {
  __typename
  ... on ConvertToDraftEvent { createdAt }
  ... on ReadyForReviewEvent { createdAt }
  ... on ReopenedEvent { createdAt }
  ... on ClosedEvent { createdAt }
  ... on MergedEvent { createdAt }
 }
 totalCount pageInfo { hasPreviousPage hasNextPage }
}
` + githubChecks
	githubListFields = githubCore + `reviewThreads(first: 100) {
 nodes { ` + githubThreadFields + ` } totalCount ` + githubPage + ` }`
	githubMineQuery = `query($search: String!, $cursor: String) {
 search(query: $search, type: ISSUE, first: 100, after: $cursor) {
  issueCount nodes { ... on PullRequest { ` + githubListFields + ` } }
  ` + githubPage + `
 }
}`
	githubBranchQuery = `query($owner: String!, $name: String!, $branch: String!) {
 repository(owner: $owner, name: $name) {
  pullRequests(headRefName: $branch, first: 50, states: [OPEN, MERGED, CLOSED],
   orderBy: {field: UPDATED_AT, direction: DESC}) {
   nodes { ` + githubListFields + ` } totalCount ` + githubPage + `
  }
 }
}`
	githubDetailQuery = `query($owner: String!, $name: String!, $number: Int!) {
 repository(owner: $owner, name: $name) {
  pullRequest(number: $number) {
   ` + githubCore + ` body
   reviews(last: 100) {
    nodes { id author { login } body state submittedAt url } totalCount ` + githubPage + `
   }
   comments(last: 100) {
    nodes { id author { login } body createdAt url } totalCount ` + githubPage + `
   }
   reviewThreads(first: 100) {
    nodes { ` + githubThreadFields + ` comments(first: 100) {
     nodes { id author { login } body createdAt url path line } totalCount ` + githubPage + `
    } } totalCount ` + githubPage + `
   }
  }
 }
}`
)

var (
	githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	githubLoginPattern      = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
)

// GitHubClient reads github.com GraphQL directly. Authentication is resolved only
// on the first request, never at construction/UI startup. Unexported dependencies
// allow offline tests; production never follows redirects with credentials.
// Construct a new client for a fresh warning/authentication scope; do not copy it.
type GitHubClient struct {
	httpClient  *http.Client
	endpoint    string
	tokenSource func(context.Context) (string, error)
	tokenOnce   sync.Once
	tokenDone   chan struct{}
	token       string
	tokenErr    error
	warningsMu  sync.Mutex
	warnings    []string
}

var _ GitHub = (*GitHubClient)(nil)

func NewGitHubClient() *GitHubClient {
	return &GitHubClient{
		httpClient:  &http.Client{Timeout: githubTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		endpoint:    githubEndpoint,
		tokenSource: githubAuthToken,
		tokenDone:   make(chan struct{}),
		warnings:    []string{},
	}
}

func githubAuthToken(ctx context.Context) (string, error) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token, nil
		}
	}
	// Discard stderr: CLI failures may contain credentials. Bound stdout too.
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com")
	var output githubTokenBuffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("GitHub authentication unavailable; set GH_TOKEN or install gh and run gh auth login --hostname github.com")
	}
	return strings.TrimSpace(output.String()), nil
}

type githubTokenBuffer struct{ bytes.Buffer }

func (b *githubTokenBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 64<<10 {
		return 0, errors.New("authentication output too large")
	}
	return b.Buffer.Write(p)
}

func (c *GitHubClient) authToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.tokenOnce.Do(func() {
		// Request cancellation must not poison credentials for the client's
		// lifetime. Resolve once under its own bounded context; every waiter
		// (including the first) can still cancel immediately.
		go func() {
			authCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), githubTimeout)
			defer cancel()
			c.token, c.tokenErr = c.tokenSource(authCtx)
			c.token = strings.TrimSpace(c.token)
			if c.tokenErr == nil && (c.token == "" || strings.ContainsAny(c.token, "\r\n")) {
				c.tokenErr = errors.New("missing or invalid token")
			}
			close(c.tokenDone)
		}()
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-c.tokenDone:
		if c.tokenErr != nil {
			if errors.Is(c.tokenErr, context.Canceled) {
				return "", context.Canceled
			}
			if errors.Is(c.tokenErr, context.DeadlineExceeded) {
				return "", context.DeadlineExceeded
			}
			// Never expose a token provider's stderr/error text.
			return "", errors.New("GitHub authentication unavailable; set GH_TOKEN or install gh and run gh auth login --hostname github.com")
		}
		return c.token, nil
	}
}

// api never includes server bodies, transport errors or auth headers in errors:
// they can echo credentials. GraphQL partial errors are not usable data.
func (c *GitHubClient) api(ctx context.Context, query string, variables map[string]any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, githubTimeout)
	defer cancel()
	token, err := c.authToken(ctx)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, errors.New("cannot encode GitHub request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("cannot create GitHub request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "herdr-prs")
	response, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("GitHub request failed; check network connectivity")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		switch response.StatusCode {
		case http.StatusUnauthorized:
			return nil, errors.New("GitHub authentication failed; set GH_TOKEN or run gh auth login --hostname github.com")
		case http.StatusForbidden:
			return nil, errors.New("GitHub access forbidden; check token permissions, repository access and rate limits")
		case http.StatusTooManyRequests:
			return nil, errors.New("GitHub rate limit exceeded; retry later")
		default:
			return nil, fmt.Errorf("GitHub request failed (HTTP %d); check repository access", response.StatusCode)
		}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, githubMaxResponse+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("cannot read GitHub response")
	}
	if len(body) > githubMaxResponse {
		return nil, errors.New("GitHub response exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var raw any
	if err := decoder.Decode(&raw); err != nil {
		return nil, errors.New("GitHub returned invalid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("GitHub returned invalid JSON")
	}
	root := githubObject(raw)
	if errs := root["errors"]; errs != nil {
		if list, ok := errs.([]any); !ok || len(list) > 0 {
			return nil, errors.New("GitHub GraphQL error; check authentication, token permissions, repository access and rate limits")
		}
	}
	data, ok := root["data"].(map[string]any)
	if !ok {
		return nil, errors.New("GitHub returned no data; check authentication and repository access")
	}
	return data, nil
}

func (c *GitHubClient) Warnings() []string {
	c.warningsMu.Lock()
	defer c.warningsMu.Unlock()
	return append([]string{}, c.warnings...)
}

func (c *GitHubClient) warn(message string, local *[]string) {
	c.warningsMu.Lock()
	defer c.warningsMu.Unlock()
	if !githubContains(c.warnings, message) {
		c.warnings = append(c.warnings, message)
	}
	if local != nil && !githubContains(*local, message) {
		*local = append(*local, message)
	}
}

func githubContains(values []string, value string) bool {
	for _, s := range values {
		if s == value {
			return true
		}
	}
	return false
}
func githubObject(value any) map[string]any { node, _ := value.(map[string]any); return node }
func githubText(value any) string           { text, _ := value.(string); return text }
func githubInt(value any) (int, bool) {
	switch n := value.(type) {
	case json.Number:
		v, err := strconv.Atoi(string(n))
		return v, err == nil
	case int:
		return n, true
	default:
		return 0, false
	}
}
func githubDefault(value any, fallback string) string {
	if text := githubText(value); text != "" {
		return text
	}
	return fallback
}
func githubBool(value any, wanted bool) bool { b, ok := value.(bool); return ok && b == wanted }

// Null/missing nodes or metadata are incomplete, never proof of no feedback.
func githubConnection(value any, limit int) ([]map[string]any, bool) {
	connection := githubObject(value)
	raw, ok := connection["nodes"].([]any)
	if !ok {
		return []map[string]any{}, false
	}
	count, validCount := githubInt(connection["totalCount"])
	complete := validCount && count == len(raw) && len(raw) <= limit && githubBool(githubObject(connection["pageInfo"])["hasNextPage"], false)
	nodes := make([]map[string]any, 0, min(len(raw), limit))
	for i, value := range raw {
		if i >= limit {
			break
		}
		if node, ok := value.(map[string]any); ok {
			nodes = append(nodes, node)
		} else {
			complete = false
		}
	}
	return nodes, complete
}

func githubRepository(repository string) (string, string, error) {
	if !githubRepositoryPattern.MatchString(repository) {
		return "", "", errors.New("expected github.com repository as owner/name")
	}
	parts := strings.Split(repository, "/")
	return parts[0], parts[1], nil
}

func (c *GitHubClient) pullRequest(node map[string]any, local *[]string) (PullRequest, bool) {
	repository := githubText(githubObject(node["repository"])["nameWithOwner"])
	number, ok := githubInt(node["number"])
	if repository == "" || !ok || number <= 0 {
		c.warn("GitHub omitted a PR identity; results are incomplete", local)
		return PullRequest{}, false
	}
	pr := PullRequest{
		Repository: repository, Number: number, Title: githubText(node["title"]), URL: githubText(node["url"]),
		Branch: githubText(node["headRefName"]), HeadRepository: githubText(githubObject(node["headRepository"])["nameWithOwner"]),
		HeadOID: githubText(node["headRefOid"]), State: githubDefault(node["state"], "UNKNOWN"), Draft: !githubBool(node["isDraft"], false),
		ReviewDecision: githubText(node["reviewDecision"]), MergeState: githubDefault(node["mergeStateStatus"], "UNKNOWN"),
		Mergeable: githubDefault(node["mergeable"], "UNKNOWN"), CreatedAt: githubText(node["createdAt"]), UpdatedAt: githubText(node["updatedAt"]),
		Links: []Link{},
	}
	switch pr.State {
	case "MERGED":
		pr.CompletedAt = githubText(node["mergedAt"])
	case "CLOSED":
		pr.CompletedAt = githubText(node["closedAt"])
	}
	pr.GroupTiming = githubGroupTiming(pr, node)
	label := fmt.Sprintf("%s#%d", repository, number)
	threads, complete := githubConnection(node["reviewThreads"], 100)
	pr.FeedbackComplete = complete
	for _, thread := range threads {
		if !githubBool(thread["isResolved"], true) {
			pr.Unresolved++
		}
		if _, ok := thread["isResolved"].(bool); !ok {
			pr.FeedbackComplete = false
		}
	}
	decision, present := node["reviewDecision"]
	validDecision := decision == nil
	if text, ok := decision.(string); ok {
		validDecision = githubContains([]string{"APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED"}, text)
	}
	if !githubContains([]string{"OPEN", "MERGED", "CLOSED"}, pr.State) || !present || !validDecision {
		pr.FeedbackComplete = false
	}
	if !pr.FeedbackComplete {
		c.warn(label+": review feedback incomplete (missing data or 100-thread limit)", local)
	}
	pr.Checks, pr.ChecksComplete = githubNormalizeChecks(node)
	if !pr.ChecksComplete {
		c.warn(label+": checks incomplete (missing data or 100-context limit)", local)
	}
	return pr, true
}

func githubNormalizeChecks(node map[string]any) ([]Check, bool) {
	checks := []Check{}
	commits, ok := githubObject(node["commits"])["nodes"].([]any)
	if !ok || len(commits) != 1 {
		return checks, false
	}
	commit := githubObject(githubObject(commits[0])["commit"])
	oid := githubText(commit["oid"])
	if oid == "" || oid != githubText(node["headRefOid"]) {
		return checks, false
	}
	rollup, present := commit["statusCheckRollup"]
	if !present {
		return checks, false
	}
	if rollup == nil {
		return checks, true
	}
	contexts, complete := githubConnection(githubObject(rollup)["contexts"], 100)
	for _, value := range contexts {
		check := Check{}
		switch githubText(value["__typename"]) {
		case "CheckRun":
			check.Name = githubText(value["name"])
			check.Status = githubDefault(value["status"], "UNKNOWN")
			check.Conclusion = githubText(value["conclusion"])
			check.URL = githubText(value["detailsUrl"])
			if check.Status == "COMPLETED" && !githubContains([]string{"SUCCESS", "NEUTRAL", "SKIPPED", "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE"}, check.Conclusion) {
				complete = false
			}
		case "StatusContext":
			check.Name = githubText(value["context"])
			check.State = githubText(value["state"])
			check.URL = githubText(value["targetUrl"])
			if !githubContains([]string{"SUCCESS", "FAILURE", "ERROR", "PENDING", "EXPECTED"}, check.State) {
				complete = false
			}
		default:
			complete = false
			continue
		}
		if check.Name == "" {
			complete = false
		}
		checks = append(checks, check)
	}
	return checks, complete
}

// Mine returns the authenticated user's authored OPEN PRs, capped at GitHub's
// 1,000-result search limit. Repeated/unusable cursors cannot loop forever.
func (c *GitHubClient) Mine(ctx context.Context) ([]PullRequest, error) {
	data, err := c.api(ctx, `query { viewer { login } }`, nil)
	if err != nil {
		return nil, err
	}
	login := githubText(githubObject(data["viewer"])["login"])
	if !githubLoginPattern.MatchString(login) {
		return nil, errors.New("GitHub did not return the authenticated user's login; run gh auth login --hostname github.com")
	}
	prs := []PullRequest{}
	seen, cursors := map[string]bool{}, map[string]bool{}
	var cursor any
	fetched := 0
	for {
		data, err = c.api(ctx, githubMineQuery, map[string]any{"search": "is:pr is:open author:" + login, "cursor": cursor})
		if err != nil {
			return nil, err
		}
		connection := githubObject(data["search"])
		raw, ok := connection["nodes"].([]any)
		if !ok {
			return nil, errors.New("GitHub returned no PR search results")
		}
		limit := min(len(raw), 100, 1000-fetched)
		for _, value := range raw[:limit] {
			pr, ok := c.pullRequest(githubObject(value), nil)
			if ok && pr.State == "OPEN" && !seen[pr.Key()] {
				prs = append(prs, pr)
				seen[pr.Key()] = true
			}
		}
		fetched += limit
		count, ok := githubInt(connection["issueCount"])
		if ok && count > 1000 {
			c.warn("My PRs incomplete: GitHub search exposes only the first 1,000 matches", nil)
		}
		if len(raw) > limit {
			c.warn("My PRs incomplete: GitHub exceeded the requested page size", nil)
			break
		}
		page := githubObject(connection["pageInfo"])
		if githubBool(page["hasNextPage"], false) {
			break
		}
		next := githubText(page["endCursor"])
		if fetched >= 1000 {
			c.warn("My PRs incomplete: GitHub search exposes only the first 1,000 matches", nil)
			break
		}
		if !githubBool(page["hasNextPage"], true) || len(raw) == 0 || next == "" || cursors[next] {
			c.warn("My PRs incomplete: GitHub omitted a usable pagination cursor", nil)
			break
		}
		cursors[next] = true
		cursor = next
	}
	return prs, nil
}

// ForBranch returns up to 50 recently updated heads in the base repository.
// Keep fork/deleted head identity intact; callers decide which checkout matches.
func (c *GitHubClient) ForBranch(ctx context.Context, repository, branch string) ([]PullRequest, error) {
	owner, name, err := githubRepository(repository)
	if err != nil {
		return nil, err
	}
	if branch == "" {
		return nil, errors.New("a nonempty head branch is required")
	}
	data, err := c.api(ctx, githubBranchQuery, map[string]any{"owner": owner, "name": name, "branch": branch})
	if err != nil {
		return nil, err
	}
	connection := githubObject(githubObject(data["repository"])["pullRequests"])
	if _, ok := connection["nodes"].([]any); !ok {
		return nil, fmt.Errorf("GitHub repository unavailable: %s", repository)
	}
	nodes, complete := githubConnection(connection, 50)
	if !complete {
		c.warn(fmt.Sprintf("%s branch %s: results incomplete (50-PR limit or missing data)", repository, branch), nil)
	}
	prs := []PullRequest{}
	for _, node := range nodes {
		pr, ok := c.pullRequest(node, nil)
		if ok && pr.Branch == branch {
			prs = append(prs, pr)
		}
	}
	return prs, nil
}

func githubComment(node map[string]any) Comment {
	return Comment{Author: githubText(githubObject(node["author"])["login"]), Body: githubText(node["body"]), URL: githubText(node["url"]), CreatedAt: githubText(node["createdAt"])}
}

func (c *GitHubClient) Detail(ctx context.Context, pr PullRequest) (Detail, error) {
	if pr.IsLocal() {
		if err := ctx.Err(); err != nil {
			return Detail{}, err
		}
		return localDetail(pr), nil
	}
	owner, name, err := githubRepository(pr.Repository)
	if err != nil {
		return Detail{}, err
	}
	if pr.Number <= 0 {
		return Detail{}, errors.New("a positive PR number is required")
	}
	data, err := c.api(ctx, githubDetailQuery, map[string]any{"owner": owner, "name": name, "number": pr.Number})
	if err != nil {
		return Detail{}, err
	}
	node := githubObject(githubObject(data["repository"])["pullRequest"])
	detail := Detail{Body: githubText(node["body"]), Reviews: []Review{}, Comments: []Comment{}, Threads: []Thread{}, Warnings: []string{}}
	fresh, ok := c.pullRequest(node, &detail.Warnings)
	if !ok {
		return Detail{}, fmt.Errorf("GitHub PR unavailable: %s#%d", pr.Repository, pr.Number)
	}
	if fresh.Key() != pr.Key() {
		return Detail{}, errors.New("GitHub returned a different PR identity")
	}
	fresh.Links = append([]Link{}, pr.Links...)
	detail.PR = fresh
	label := fmt.Sprintf("%s#%d", pr.Repository, pr.Number)
	if body, present := node["body"]; !present || body == nil {
		c.warn(label+": PR body unavailable", &detail.Warnings)
	}
	reviews, complete := githubConnection(node["reviews"], 100)
	for _, review := range reviews {
		comment := githubComment(review)
		detail.Reviews = append(detail.Reviews, Review{Author: comment.Author, Body: comment.Body, State: githubText(review["state"]), URL: comment.URL, SubmittedAt: githubText(review["submittedAt"])})
	}
	if !complete {
		c.warn(label+": reviews incomplete (100-item limit or missing data)", &detail.Warnings)
	}
	comments, complete := githubConnection(node["comments"], 100)
	for _, comment := range comments {
		detail.Comments = append(detail.Comments, githubComment(comment))
	}
	if !complete {
		c.warn(label+": comments incomplete (100-item limit or missing data)", &detail.Warnings)
	}
	threads, _ := githubConnection(node["reviewThreads"], 100)
	for _, thread := range threads {
		if githubBool(thread["isResolved"], true) {
			continue
		}
		line, _ := githubInt(thread["line"])
		item := Thread{Path: githubText(thread["path"]), Line: line, Resolved: false, Outdated: githubBool(thread["isOutdated"], true), Comments: []Comment{}}
		comments, complete := githubConnection(thread["comments"], 100)
		for _, comment := range comments {
			item.Comments = append(item.Comments, githubComment(comment))
		}
		detail.Threads = append(detail.Threads, item)
		if !complete || len(comments) == 0 {
			c.warn(label+": thread "+githubText(thread["id"])+" comments incomplete (100-item limit or missing data)", &detail.Warnings)
		}
	}
	return detail, nil
}
