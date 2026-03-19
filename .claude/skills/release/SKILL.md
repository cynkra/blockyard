---
name: release
description: Create a new release by tagging main and monitoring the release workflow.
---

# Create Release

Create a release for version $ARGUMENTS (e.g., `v1.2.0`).

## Pre-flight

1. Ensure you're on `main` and up to date:
   ```sh
   git fetch origin main
   git log --oneline origin/main..HEAD  # must be empty
   ```
2. Check that CI is green on the latest main commit:
   ```sh
   gh api repos/cynkra/blockyard/commits/main/check-runs \
     --jq '.check_runs[] | "\(.name) \(.conclusion)"'
   ```

## Tag & Push

```sh
git tag -a <version> -m "Release <version>"
git push origin <version>
```

## Monitor Release Workflow

The release workflow runs: CI -> e2e -> multi-arch Docker image ->
manifest -> GitHub Release with binaries.

Poll the release run:

```sh
gh api repos/cynkra/blockyard/actions/runs?event=push&branch=<version>&per_page=1 \
  --jq '.workflow_runs[0] | "\(.id) \(.status) \(.conclusion // "running")"'
```

Check job progress:

```sh
gh run view <id> --json jobs --jq '.jobs[] | "\(.name) \(.status) \(.conclusion // "running")"'
```

## On Failure

1. Get logs: `gh run view <id> --log-failed`
2. Diagnose and fix.
3. If fixable: delete the tag, fix, re-tag, re-push:
   ```sh
   git push --delete origin <version>
   git tag -d <version>
   # ... fix and commit ...
   git tag -a <version> -m "Release <version>"
   git push origin <version>
   ```

## Verify

Once complete, confirm the release:

```sh
gh release view <version>
```

Report the release URL to the user.
