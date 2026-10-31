# skills — Agent Skills for coding agents

Canonical location of this repo's [Agent Skills](https://agentskills.io) (`<name>/SKILL.md`, frontmatter
`name` + `description`). Edit them here only; agents read them through a symlink.

| Skill | Triggers on |
|---|---|
| [`trooper-diagnose`](trooper-diagnose/SKILL.md) | "something is wrong / slow / erroring with X" → incident → KPIs → endpoint RCA → traces → logs → deploys |
| [`trooper-query`](trooper-query/SKILL.md) | questions about production data; how to scope, paginate and read `tt_*` results |
| [`trooper-contribute`](trooper-contribute/SKILL.md) | changing this codebase; checklist pointing into `AGENTS.md` and `.ai/` |

`trooper-diagnose` and `trooper-query` need the `tasktrooper` MCP server (`services/mcp`, 12 read-only `tt_*`
tools). Setup, tool contract and agent configs: [`.ai/mcp.md`](../.ai/mcp.md) § Bağlanma and § Agent Skills.

## Discovery

`.claude/skills -> ../skills` (committed symlink) is the only wiring:

- **Claude Code** loads project skills from `.claude/skills/<name>/SKILL.md`.
- **OpenCode** (≥ 2.0) scans `.opencode/skill(s)/`, and for compatibility `.claude/skills/` and
  `.agents/skills/`, walking up from the working directory; symlinks are followed.

Windows / no-symlink checkouts (`core.symlinks=false`): point OpenCode at the directory in `opencode.json`
(`"skills": ["./skills"]`); for Claude Code replace the placeholder with a copy and don't commit it:
`rm .claude/skills && cp -r skills .claude/skills`.

Quick check: `claude -p "list skills starting with trooper-"`; in OpenCode the `/skills` command lists them.
