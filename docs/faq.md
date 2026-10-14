# FAQ

[← README](../README.md)

## Why is a PR missing or unlinked?

Your authored open PRs appear across repositories. Other authors' PRs need a
matching checkout in the active Herdr session; repositories elsewhere on disk
are not scanned.

| Case | Behavior |
| --- | --- |
| No active workspace/pane | PR can appear, but cannot be navigated to |
| Ambiguous remote/branch or custom push refspec | Remains unlinked with a warning |
| Detached checkout | Cannot link to a PR |
| Confirmed default branch without an open PR | No local-work or completed-history row |
| Failed/truncated GitHub reads | Warning shown; available data retained |

Linking requires the exact head repository and branch, not just a matching branch
name. Navigation rechecks the target; it never creates worktrees or starts agents.

## Why does completed work remain?

An active feature checkout with no open PR shows its latest merged/closed PR.
The row disappears when that checkout leaves the session. Completed history is
not imported for confirmed default branches.

## What do the agent icons mean?

These match Herdr 0.9.3's default dot indicators with the Nord palette:

| Icon | Agent state |
| --- | --- |
| Amber `●` | Working |
| Teal `●` | Unread completion |
| Green `○` | Idle |
| Red `●` | Blocked on approval/question |
| Dim `·` | Present, state unknown |
| Blank | No agent |

They aggregate agents in the preferred navigation worktree, not unrelated
checkouts of the same PR. Priority is blocked → unread → working → idle → unknown.
States remain separate from GitHub workflow groups and statuses. Reads do not
acknowledge completion or start/focus agents. Herdr exposes server unread state;
a TUI client tracks viewed completions independently, so its sidebar may differ.

## What does age mean?

| Age | Meaning |
| --- | --- |
| PR row | Time in the current workflow group, based on GitHub lifecycle events |
| Local row | Approximate first-seen time for that worktree/branch |
| Details: **created** | Time since PR creation |
| Blank | History unavailable |

Comments and CI/feedback changes do not reset group age. Local clocks persist
across opens; a different branch starts a new observation. Seconds appear only
below one minute. Hours/days include a nonzero second unit without padding:
`59s`, `1m`, `1h`, `1h3m`, `1d`, `1d2h`; never `1h0m` or `1d0h`.

## When does data refresh?

Opening rediscovers checkouts and shows cached GitHub data first. Entries under
60 seconds old skip reads; older data refreshes while remaining visible.

- <kbd>Ctrl</kbd> + <kbd>R</kbd> forces refresh/retry.
- Failed refreshes retain available data and show errors.
- `--list` reads live, without creating local timing observations.
- GitHub is not polled automatically. Herdr agent states refresh read-only each second.

GitHub statuses are informational: **ready is not permission to merge**.
Incomplete readiness data never yields ready.

## What private content is stored?

> [!WARNING]
> PR titles, status, descriptions, reviews, and comments are cached locally,
> including private repository content. Tokens and navigation links are not.

GitHub caches are isolated by authentication token. Local first-seen clocks are
independent of authentication and store only timestamps/schema data under hashed
worktree/branch filenames, with no identifying names or paths.

Delete the [cache directory](../README.md#privacy-and-cache) to clear content and
local clocks. GitHub entries older than seven days are ignored, not automatically
deleted; local clocks outlive that limit.
