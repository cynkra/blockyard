---
name: pr
description: Create a pull request from the current branch's changes. Handles branch naming, committing, pushing, PR creation, and CI monitoring.
---

# Create Pull Request

Create a PR for the current changes. If arguments are provided, use them
as context for the PR title/description: $ARGUMENTS

## Branch

If on `main`, create a new branch first. Branch names MUST follow the
repo convention enforced by the GitHub ruleset:

```
<type>/<short-description>
```

Types: `feat`, `fix`, `ci`, `docs`, `refactor`, `chore`.
Use lowercase, hyphen-separated descriptions. Keep it short.

## Commit

1. Run `git status` and `git diff` to understand all changes.
2. Write a concise commit message (1-2 sentences) focused on the "why".
3. Stage specific files — never `git add -A`. Never commit `.env` or credentials.
4. Commit using a HEREDOC for the message.

## Push & Create PR

1. Push with `-u origin <branch>`.
2. Create the PR with `gh pr create`. Keep the body to a short `## Summary`
   section with 2-5 bullet points. No checklists, no test plans, no boilerplate.
3. Use a HEREDOC for the body to ensure correct formatting.

## Monitor CI

After pushing, poll CI until all checks resolve:

```sh
gh pr checks <number>
```

If any check fails:
1. Get logs: `gh run view <id> --log-failed`
2. Diagnose and fix the issue.
3. Commit, push, and monitor again.

Repeat until all checks pass. Do NOT ask the user to check CI — that is
your responsibility.
