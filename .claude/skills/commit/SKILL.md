---
name: commit
description: Commit current changes. Creates a branch if on main. Use for incremental work before opening a PR.
---

# Commit Changes

Commit the current changes: $ARGUMENTS

## Branch Decision

Check the current branch and decide:

1. **On `main`**: Always create a new branch from `main`. Pick a name
   following the repo convention (`<type>/<short-description>`).
   Types: `feat`, `fix`, `ci`, `docs`, `refactor`, `chore`.
2. **On a branch that has an open PR**: Stay on it — this is ongoing
   work on that PR.
3. **On a branch without a PR**: Check if the staged changes are
   related to the branch's purpose (look at branch name and recent
   commits). If related, stay. If clearly unrelated, ask the user
   whether to commit here or create a new branch.

When creating a new branch, always fetch and branch from `origin/main`
to ensure a clean base:
```sh
git fetch origin main
git checkout -b <type>/<name> origin/main
```

## Commit

1. Run `git status` and `git diff` to understand all changes.
2. Write a concise commit message (1-2 sentences) focused on the "why".
3. Stage specific files — never `git add -A`. Never commit `.env` or
   credentials. Use `git add -f` for files covered by gitignore that
   should be tracked (e.g., `.claude/skills/`).
4. Commit using a HEREDOC:
   ```sh
   git commit -m "$(cat <<'EOF'
   Message here.
   EOF
   )"
   ```

## Push

Push with `-u origin <branch>` so the remote is tracked.

Do NOT create a PR — use `/pr` for that.
