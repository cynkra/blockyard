---
name: commit
description: Commit current changes. Creates a branch if on main. Use for incremental work before opening a PR.
---

# Commit Changes

Commit the current changes: $ARGUMENTS

## Branch

If on `main`, create a new branch first. Branch names MUST follow the
convention enforced by the GitHub ruleset:

```
<type>/<short-description>
```

Types: `feat`, `fix`, `ci`, `docs`, `refactor`, `chore`.
Lowercase, hyphen-separated. Keep it short.

If already on a non-main branch, stay on it.

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
