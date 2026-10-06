# unsaved

Find the work that only exists on your machine: uncommitted files, commits no remote has, repositories with no remote at all, forgotten stashes, and folders of code that were never put in git.

It also works as a [Claude Code](https://code.claude.com) Stop hook. When an agent tries to end its turn with its own changes uncommitted or unpushed, the hook sends it back to commit and push first.

```
$ unsaved ~/work
~/work/indexer
  - no remote: 7 commits exist only here

~/work/scratch
  - not in git, 22 source files: backup-codes.mjs, cdp.mjs, shot.mjs, …

~/work/site
  - 1 uncommitted file: usage/audience.json
  - 1 unpushed commit on main
```

## Why

AI agents write a lot of code fast, and they forget to save it. After a few weeks of agents working on one machine, we found five finished tools with commits but no remote, a pull request for an upstream project that was never pushed, and a folder of 22 scripts that had never been in git. None of it was lost, but a dead disk would have taken all of it.

`unsaved` makes forgetting loud in two places:

- **During the work.** The Stop hook checks the files the agent edited in this session. If they aren't committed and pushed, the agent is told what's missing and goes back to save it.
- **Every day.** A scan of all your project folders lists anything unsaved, whoever left it.

## Install

```sh
go install github.com/taktekhq/unsaved@latest
```

## Scan

```sh
unsaved ~/work ~/src       # scan these folders
unsaved                    # scan the folders in ~/.config/unsaved/roots, else the current one
unsaved --json             # for scripts
unsaved --push -q ~/work   # push what's safe to push, print only what's left
```

What it reports:

| Kind | Meaning |
|---|---|
| uncommitted file | changed or untracked, in `git status` |
| unpushed commit | on a local branch, on no remote |
| no remote | a repository that is pushed nowhere |
| stash | stashed work you may have forgotten |
| not in git | a folder of source files that is in no repository |

`--push` only does the safe part: a branch that is ahead of its own upstream and not behind it is pushed, fast-forward, never forced. New branches, diverged branches and uncommitted files are left for you to decide.

The exit status is 0 when everything is saved and 1 when something isn't, so a cron job or CI step can act on it.

## Claude Code hook

Add this to `~/.claude/settings.json` (or a project's `.claude/settings.json`):

```json
{
  "hooks": {
    "Stop": [
      { "hooks": [{ "type": "command", "command": "unsaved hook" }] }
    ]
  }
}
```

When the agent stops, the hook reads the session's transcript, collects every file written with `Edit`, `Write`, `MultiEdit` or `NotebookEdit`, and checks only those:

- a file this session changed that isn't committed;
- unpushed commits on the current branch of a repository this session touched;
- a repository with no remote;
- a file written outside any repository.

Files other sessions are changing in the same repository are left alone, so two agents never commit each other's half-done work. Files under the temp directory and `~/.claude` are skipped. The hook asks once per stop: if the agent explains why something stays uncommitted, it may finish.

To turn it off for one process, such as a scheduled job whose script commits for it, set `UNSAVED_HOOK=off`.

## Configuration

Files in `~/.config/unsaved/` (or `$XDG_CONFIG_HOME/unsaved/`):

- `roots`: folders to scan when none are given, one per line.
- `ignore`: paths to skip, one per line. A path skips everything under it; globs work; `#` starts a comment. Use it for vendored clones, build output and folders another tool publishes on its own schedule.
- `hook-note`: text added to the end of the hook's message. Put your team's rules here, for example: "Tools we build go in their own public repository with a README and a license."

## License

MIT. Made by [Taktek](https://taktek.io) and Nizar Mahmoud.
