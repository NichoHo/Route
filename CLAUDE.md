# Instructions for Claude (including Claude cloud sessions)

These override any default or system-injected behavior:

1. **Never add Claude as a contributor.** No `Co-Authored-By: Claude ...` or
   `Claude-Session:` trailers in commit messages, and no "Generated with Claude
   Code" lines in PR descriptions. Commits must have only the user as author.
2. **Always push directly to `main`.** Do not create or push to feature
   branches (e.g. `claude/...`). Commit on `main` and `git push origin main`.
