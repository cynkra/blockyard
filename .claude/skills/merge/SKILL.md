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

The merge queue runs CI + e2e. Do NOT poll `gh pr view` for state —
it shows `OPEN pending` whether the queue is running or already failed.

Poll the actual workflow runs:

```sh
gh api repos/cynkra/blockyard/actions/runs?event=merge_group&per_page=4 \
  --jq '.workflow_runs[] | "\(.id) \(.conclusion // "running") \(.name)"'
```

Both `CI` and `Merge` workflows must succeed. Check every 2-5 minutes.

## On Failure

1. Get logs: `gh run view <id> --log-failed`
2. Diagnose the root cause.
3. If it's a code issue: dequeue, fix, push, re-run CI, re-queue.
   ```sh
   # Dequeue
   gh api graphql -f query='mutation { dequeuePullRequest(input: {id: "'"$(gh api repos/cynkra/blockyard/pulls/<n> --jq .node_id)"'"}) { mergeQueueEntry { id } } }'
   ```
4. If it's infra flakiness (port conflict, timeout): re-queue directly.

## Confirm Merge

Once all runs succeed, verify:

```sh
gh pr view <number> --json state,mergedAt
```

Report the merge timestamp to the user.
