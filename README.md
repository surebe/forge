# forge — a GitHub-compatible git forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/surebe/forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/surebe/forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

Built for the [make.fun `github-core-clone-01` bounty](https://make.fun/bounty/github-core-clone-01) — "Build a GitHub-compatible git forge", $500 in USDC.

A single-process, in-memory implementation of the GitHub surfaces the
`github-core-clone-01` bounty exercises, written from scratch in Go using only
the standard library:

- **REST v3** — repositories, contents (backed by the real git object graph,
  with blob-sha optimistic locking), labels, issues, comments, pull requests
  with squash merges, branches, refs, commits, Link-header pagination.
- **Git smart HTTP** — protocol v2 `ls-refs`/`fetch` (with shallow/deepen
  semantics), v0 advertisement and fetch fallback, and `git-receive-pack`
  with report-status in side-band-64k. Objects are stored and served
  byte-verbatim; one object store backs every surface.
- **GraphQL** — `/api/graphql` with introspection for the checked types,
  cursor-paginated issues/pulls, search, and the create/close/merge mutations.
- **MCP** — `/mcp` JSON-RPC 2.0 (revision 2026-07-28, `server/discover`),
  six tools over the same store as REST.

## Running

The grader runs `node main.ts`. The wrapper spawns the committed static
binary `./forge` and forwards the environment and exit code. The binary binds
an ephemeral port, registers it with `$MAKEFUN_ENV_URL/register`, serves until
`$MAKEFUN_ENV_URL/done` reports the trial is over, and exits 0. Without
`MAKEFUN_ENV_URL` it serves indefinitely (local development).

Authentication is `Authorization: Bearer $MAKEFUN_SHOP_TOKEN`. All state is in
memory and deterministic: timestamps derive from a sequence clock seeded at
the scenario epoch, so identical request streams produce identical responses.

## Rebuilding

```sh
CGO_ENABLED=0 go build -o forge .
```

Requires Go 1.24+. No third-party dependencies.
