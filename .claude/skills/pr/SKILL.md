---
name: pr
description: Create a pull request from the current branch. Handles pushing, PR creation, and CI monitoring. Use /commit first to stage changes.
---

# Create Pull Request

Create a PR from the current branch: $ARGUMENTS

## Pre-flight

1. Verify you're NOT on `main`. If on main, abort and tell the user
   to use `/commit` first.
2. Check for uncommitted changes — commit them first if present.
3. Push with `-u origin <branch>` if not already pushed.

## Create PR

```sh
gh pr create --title "<title>" --body "$(cat <<'EOF'
## Summary
<2-5 bullet points>
EOF
)"
```

- Title: under 70 characters, describes the change.
- Body: short `## Summary` section only. No checklists, no test plans,
  no boilerplate, no "Generated with Claude Code".

## Monitor CI

After creating the PR, poll CI until all checks resolve:

```sh
gh pr checks <number>
```

If any check fails:
1. Get logs: `gh run view <id> --log-failed`
2. Diagnose and fix the issue.
3. Commit, push, and monitor again.

Repeat until all checks pass. Do NOT ask the user to check CI — that
is your responsibility.
