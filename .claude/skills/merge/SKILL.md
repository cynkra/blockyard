---
name: merge
description: Queue a pull request for merging via the merge queue and monitor until it lands on main.
---

# Merge Pull Request

Queue PR $ARGUMENTS for merging and monitor until it lands.

## Queue

```sh
gh pr merge <number> --auto
```

## Monitor

CRITICAL: Do NOT poll `gh pr view` — it shows `OPEN pending` whether
the queue is running or already failed. Poll the workflow runs directly,
every 2-3 minutes:

```sh
gh api repos/cynkra/blockyard/actions/runs?event=merge_group&per_page=4 \
  --jq '.workflow_runs[] | "\(.id) \(.conclusion // "running") \(.name)"'
```

Both `CI` and `Merge` workflows must show `success`. If EITHER shows
`failure`, investigate immediately — do not wait.

```sh
# Check which jobs failed
gh run view <id> --json jobs --jq '.jobs[] | "\(.name) \(.conclusion)"'

# Get failure logs
gh run view <id> --log-failed
```

## On Failure

1. Diagnose the root cause from the logs.
2. If it's a code issue: dequeue, fix, push, wait for PR CI, re-queue.
   ```sh
   # Dequeue
   gh api graphql -f query='mutation { dequeuePullRequest(input: {id: "'"$(gh api repos/cynkra/blockyard/pulls/<n> --jq .node_id)"'"}) { mergeQueueEntry { id } } }'
   ```
3. If it's infra flakiness (port conflict, timeout): re-queue directly.
4. After fixing and re-queuing, resume monitoring from the top.

## Confirm Merge

Only after BOTH workflow runs show `success`, verify:

```sh
gh pr view <number> --json state,mergedAt
```

Report the merge timestamp to the user.
