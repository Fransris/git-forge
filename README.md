# git-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Fransris/git-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Fransris/git-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, GitHub-compatible git forge: one Node 22 process, standard library only, serving git
smart HTTP, the REST v3 API, the GraphQL v4 API and MCP over Streamable HTTP from one store.

## Run it

```sh
MAKEFUN_ENV_URL=http://127.0.0.1:8080 MAKEFUN_SHOP_TOKEN=shpat_... node main.ts
```

---

## What it serves

- **git smart HTTP** at `/{owner}/{repo}.git` — upload-pack protocol **v2** (ls-refs with `peel`,
  `symrefs`, `unborn` and `ref-prefix`; fetch with `shallow-info`, acknowledgments and side-band
  packfile) and receive-pack **v0** (report-status over side-band-64k, delete-refs, fast-forward
  enforcement per ref, mixed reports). The packfile reader resolves OFS_DELTA and REF_DELTA and
  verifies the trailer; the writer emits plain objects, which every client accepts.
- **REST v3** under `/api/v3` — repositories, branches, commits, contents (create, update, stale-sha
  409, delete, directory listing), labels, issues, comments, pull requests, their files and squash
  merges (404 unknown, 405 conflicted), with Link pagination and a JSON 404 for every unknown route.
- **GraphQL v4** at `/api/graphql` — a real parser and executor for the documents `gh` and
  `github-mcp-server` send: aliases, variables, inline enums and input objects, nested selections,
  `... on Type`, `__type` introspection, issue connections with cursors, and the create/close/merge
  mutations. Batched operation arrays answer in order.
- **MCP** at `/mcp` — JSON-RPC 2.0 over Streamable HTTP in both protocol eras: `server/discover` with
  `supportedVersions` for 2026-07-28, and `initialize` plus `Mcp-Session-Id` for the handshake era.
  Seven tools, stable `tools/list` output, `isError` on a bad call, -32601 for an unknown tool.

## How it is built

- `store.ts` — the store: a deterministic clock (fixed epoch plus a tick), repositories, issues,
  pulls, labels and the git object graph. Nothing reads the wall clock, so two runs on the same
  instance leave the same export.
- `git.ts` — the object model: sha1 object ids, tree building and flattening, commit parsing,
  reachability, merge base, packfile read and write, delta application, pkt-lines, shallow walks.
- `githttp.ts` — the protocol: advertisements, ls-refs, fetch with shallow support, receive-pack with
  per-ref reports. A fetch and an ls-refs are cached by their exact question, because fifty shallow
  clones of one branch ask the same question.
- `rest.ts`, `graphql.ts`, `mcp.ts` — the three API surfaces, all reading and writing that one store.

The login is not configured anywhere: the forge serves a single account and adopts the owner from the
first repository path the client asks for, so the token's own requests define the account.

## License

MIT — see `LICENSE`.

---

<sub>Entered on [make.fun](https://make.fun/?ref=zydymsxs&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
