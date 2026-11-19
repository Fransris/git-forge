// MCP over Streamable HTTP: JSON-RPC 2.0 at /mcp, both the 2026-07-28 revision (server/discover, no
// handshake) and the handshake era (initialize, notifications/initialized, Mcp-Session-Id). The tools
// act on the same store REST reads, so an object created here reads back over /api/v3 unchanged.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { store, type Issue, type Pull, type Repo } from './store.ts';
import { buildCommit, flattenTree, mergeBase, parseCommit, putObject, readObject } from './git.ts';
import { sendJson } from './rest.ts';

export interface McpContext {
  req: IncomingMessage;
  res: ServerResponse;
  method: string;
  body: Buffer;
  json: () => unknown;
  token: string | null;
}

const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26'];
const SERVER_INFO = { name: 'makefun-git-forge', version: '1.0.0' };

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
}

const str = (description: string): Record<string, unknown> => ({ type: 'string', description });

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a repository on this forge. Returns the repository object REST reads at /api/v3/repos/{owner}/{repo}.',
    inputSchema: {
      type: 'object',
      properties: { name: str('Repository name'), description: str('Description'), private: { type: 'boolean', description: 'Whether the repository is private' } },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Read one repository by owner and name.',
    inputSchema: { type: 'object', properties: { owner: str('Repository owner login'), repo: str('Repository name'), full_name: str('owner/repo') }, required: ['owner', 'repo'] },
  },
  {
    name: 'create_issue',
    description: 'Open an issue in a repository. Returns the issue object REST reads at /api/v3/repos/{owner}/{repo}/issues/{number}.',
    inputSchema: { type: 'object', properties: { owner: str('Repository owner'), repo: str('Repository name'), title: str('Issue title'), body: str('Issue body'), full_name: str('owner/repo') }, required: ['owner', 'repo', 'title'] },
  },
  {
    name: 'list_issues',
    description: 'List a repository\'s issues, open by default, newest first.',
    inputSchema: { type: 'object', properties: { owner: str('Repository owner'), repo: str('Repository name'), state: str('open, closed or all'), full_name: str('owner/repo') }, required: ['owner', 'repo'] },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch that exists in the repository.',
    inputSchema: { type: 'object', properties: { owner: str('Repository owner'), repo: str('Repository name'), title: str('Pull request title'), head: str('Head branch'), base: str('Base branch'), body: str('Body'), full_name: str('owner/repo') }, required: ['owner', 'repo', 'title', 'head', 'base'] },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge an open pull request into its base branch.',
    inputSchema: { type: 'object', properties: { owner: str('Repository owner'), repo: str('Repository name'), pull_number: { type: 'number', description: 'Pull request number' }, full_name: str('owner/repo') }, required: ['owner', 'repo', 'pull_number'] },
  },
  {
    name: 'create_comment',
    description: 'Comment on an issue and return the comment object.',
    inputSchema: { type: 'object', properties: { owner: str('Repository owner'), repo: str('Repository name'), issue_number: { type: 'number', description: 'Issue number' }, body: str('Comment body'), full_name: str('owner/repo') }, required: ['owner', 'repo', 'issue_number', 'body'] },
  },
];

const TOOLS_JSON = TOOLS.map((tool) => ({ ...tool }));

function textResult(payload: unknown): Record<string, unknown> {
  return { content: [{ type: 'text', text: JSON.stringify(payload) }], structuredContent: payload, isError: false };
}

function errorResult(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function repoOf(args: Record<string, unknown>): Repo | null {
  const full = typeof args.full_name === 'string' ? args.full_name : '';
  const owner = typeof args.owner === 'string' && args.owner !== '' ? args.owner : (full.split('/')[0] ?? '');
  const repo = typeof args.repo === 'string' && args.repo !== '' ? args.repo : (full.split('/')[1] ?? '');
  if (owner === '' || repo === '') return null;
  store.learnOwner(owner);
  return store.find(owner, repo);
}

function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((l) => l.name),
    created_at: issue.created_at,
    updated_at: issue.updated_at,
    repository: `${repo.owner}/${repo.name}`,
  };
}

function pullJson(repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merge_commit_sha: pull.merge_commit_sha,
    head: pull.head,
    base: pull.base,
    repository: `${repo.owner}/${repo.name}`,
  };
}

/** Squash-merge, the same path REST's merge takes. */
function mergePull(repo: Repo, pull: Pull): { ok: true; sha: string } | { ok: false; reason: string } {
  if (pull.merged) return { ok: false, reason: 'already merged' };
  const headTip = repo.refs.get(`refs/heads/${pull.head.ref}`) ?? null;
  const baseTip = repo.refs.get(`refs/heads/${pull.base.ref}`) ?? null;
  if (headTip === null || baseTip === null) return { ok: false, reason: 'the branch is gone' };
  const common = mergeBase(repo, baseTip, headTip);
  if (common !== null) {
    const changed = (from: string, to: string): Set<string> => {
      const before = flattenTree(repo, parseCommit(readObject(repo, from)!.data).tree);
      const after = flattenTree(repo, parseCommit(readObject(repo, to)!.data).tree);
      const out = new Set<string>();
      for (const [p, v] of after) if (before.get(p)?.sha !== v.sha) out.add(p);
      for (const [p] of before) if (!after.has(p)) out.add(p);
      return out;
    };
    const onBase = changed(common, baseTip);
    const onHead = changed(common, headTip);
    if ([...onHead].some((p) => onBase.has(p))) return { ok: false, reason: 'the pull request is not mergeable' };
  }
  const headObject = readObject(repo, headTip);
  if (headObject === null) return { ok: false, reason: 'the head commit is missing' };
  const tree = parseCommit(headObject.data).tree;
  const time = store.clock.tick();
  const author = { name: repo.owner, email: `${repo.owner}@example.invalid` };
  const sha = putObject(repo, 'commit', buildCommit({ tree, parents: [baseTip], message: `Merge pull request #${pull.number} from ${pull.head.ref}\n\n${pull.title}`, author, time }));
  repo.refs.set(`refs/heads/${pull.base.ref}`, sha);
  repo.packCache.clear();
    repo.bodyCache.clear();
  pull.state = 'closed';
  pull.merged = true;
  pull.merged_at = time;
  pull.closed_at = time;
  pull.merge_commit_sha = sha;
  pull.updated_at = time;
  return { ok: true, sha };
}

function callTool(name: string, args: Record<string, unknown>): Record<string, unknown> {
  const bare = name.includes('__') ? name.slice(name.lastIndexOf('__') + 2) : name;
  const tool = bare.toLowerCase();

  if (tool === 'create_repo') {
    if (typeof args.name !== 'string' || args.name === '') return errorResult('name is required');
    try {
      const repo = store.createRepo(args.name, typeof args.description === 'string' ? args.description : '', args.private === true);
      return textResult({ id: repo.id, name: repo.name, owner: repo.owner, full_name: `${repo.owner}/${repo.name}`, private: repo.private, default_branch: repo.default_branch, description: repo.description });
    } catch {
      return errorResult('a repository with that name already exists');
    }
  }

  if (tool === 'get_repo') {
    const repo = repoOf(args);
    if (repo === null) return errorResult('repository not found');
    return textResult({ id: repo.id, name: repo.name, owner: repo.owner, full_name: `${repo.owner}/${repo.name}`, private: repo.private, default_branch: repo.default_branch, description: repo.description, open_issues: repo.issues.filter((i) => i.state === 'open').length });
  }

  if (tool === 'create_issue') {
    const repo = repoOf(args);
    if (repo === null) return errorResult('repository not found');
    if (typeof args.title !== 'string' || args.title === '') return errorResult('title is required');
    const now = store.clock.tick();
    const issue: Issue = { id: store.newIssueId(), number: repo.next_issue++, title: args.title, body: typeof args.body === 'string' ? args.body : '', state: 'open', labels: [], created_at: now, updated_at: now, closed_at: null, comments: [] };
    repo.issues.push(issue);
    return textResult(issueJson(repo, issue));
  }

  if (tool === 'list_issues') {
    const repo = repoOf(args);
    if (repo === null) return errorResult('repository not found');
    const state = typeof args.state === 'string' && args.state !== '' ? args.state : 'open';
    const list = repo.issues.filter((i) => state === 'all' || i.state === state).sort((a, b) => b.number - a.number);
    return textResult({ total: list.length, issues: list.map((i) => issueJson(repo, i)) });
  }

  if (tool === 'create_pull_request') {
    const repo = repoOf(args);
    if (repo === null) return errorResult('repository not found');
    const headRef = typeof args.head === 'string' ? args.head : '';
    const baseRef = typeof args.base === 'string' && args.base !== '' ? args.base : repo.default_branch;
    const headTip = repo.refs.get(`refs/heads/${headRef}`) ?? null;
    const baseTip = repo.refs.get(`refs/heads/${baseRef}`) ?? null;
    if (headTip === null || baseTip === null) return errorResult('the head or base branch does not exist');
    const now = store.clock.tick();
    const pull: Pull = {
      id: store.newPullId(),
      number: repo.next_pull++,
      title: typeof args.title === 'string' ? args.title : '',
      body: typeof args.body === 'string' ? args.body : '',
      state: 'open',
      head: { ref: headRef, sha: headTip },
      base: { ref: baseRef, sha: baseTip },
      created_at: now,
      updated_at: now,
      closed_at: null,
      merged: false,
      merged_at: null,
      merge_commit_sha: null,
      mergeable: true,
      files: [],
    };
    repo.pulls.push(pull);
    return textResult(pullJson(repo, pull));
  }

  if (tool === 'merge_pull_request') {
    const repo = repoOf(args);
    if (repo === null) return errorResult('repository not found');
    const number = Number(args.pull_number);
    const pull = repo.pulls.find((p) => p.number === number);
    if (pull === undefined) return errorResult('pull request not found');
    const merged = mergePull(repo, pull);
    if (!merged.ok) return errorResult(merged.reason);
    return textResult({ ...pullJson(repo, pull), merged: true, sha: merged.sha, message: 'Pull Request successfully merged' });
  }

  if (tool === 'create_comment') {
    const repo = repoOf(args);
    if (repo === null) return errorResult('repository not found');
    const number = Number(args.issue_number);
    const issue = repo.issues.find((i) => i.number === number);
    if (issue === undefined) return errorResult('issue not found');
    if (typeof args.body !== 'string' || args.body === '') return errorResult('body is required');
    const now = store.clock.tick();
    const comment = { id: store.newCommentId(), body: args.body, created_at: now, updated_at: now };
    issue.comments.push(comment);
    issue.updated_at = now;
    return textResult({ id: comment.id, body: comment.body, issue_number: issue.number, created_at: now });
  }

  return errorResult(`unknown tool: ${name}`);
}

function rpcResult(id: unknown, result: unknown): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, result };
}

function rpcError(id: unknown, code: number, message: string): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, error: { code, message } };
}

function declaredVersion(message: Record<string, unknown>, req: IncomingMessage): string {
  const meta = message._meta;
  if (meta !== null && typeof meta === 'object') {
    const declared = (meta as Record<string, unknown>)['io.modelcontextprotocol/protocolVersion'];
    if (typeof declared === 'string' && declared !== '') return declared;
    const plain = (meta as Record<string, unknown>).protocolVersion;
    if (typeof plain === 'string' && plain !== '') return plain;
  }
  const header = req.headers['mcp-protocol-version'];
  if (typeof header === 'string' && header !== '') return header;
  return SUPPORTED_VERSIONS[0]!;
}

function handleMessage(message: Record<string, unknown>, req: IncomingMessage): Record<string, unknown> | null {
  const id = message.id ?? null;
  const method = typeof message.method === 'string' ? message.method : '';
  const params = (message.params !== null && typeof message.params === 'object' ? message.params : {}) as Record<string, unknown>;
  const isNotification = message.id === undefined || message.id === null;

  if (method.startsWith('notifications/')) return null;

  if (method === 'server/discover') {
    return rpcResult(id, {
      resultType: 'discover',
      supportedVersions: SUPPORTED_VERSIONS,
      capabilities: { tools: { listChanged: false } },
      serverInfo: SERVER_INFO,
      instructions: 'A GitHub-compatible forge: create repositories, issues and pull requests, and merge them.',
    });
  }

  if (method === 'initialize') {
    const offered = params.protocolVersion;
    const chosen = typeof offered === 'string' && SUPPORTED_VERSIONS.includes(offered) ? offered : SUPPORTED_VERSIONS[SUPPORTED_VERSIONS.length - 1]!;
    return rpcResult(id, { protocolVersion: chosen, capabilities: { tools: { listChanged: false } }, serverInfo: SERVER_INFO });
  }

  if (method === 'ping') return rpcResult(id, {});

  if (method === 'tools/list') return rpcResult(id, { tools: TOOLS_JSON });

  if (method === 'tools/call') {
    const name = typeof params.name === 'string' ? params.name : '';
    const args = (params.arguments !== null && typeof params.arguments === 'object' ? params.arguments : {}) as Record<string, unknown>;
    if (name === '') return rpcError(id, -32602, 'Invalid params: name is required');
    const known = TOOLS.find((t) => t.name === name || name.toLowerCase().endsWith(`__${t.name}`));
    if (known === undefined) return rpcError(id, -32601, `Method not found: ${name}`);
    const required = (known.inputSchema.required ?? []) as string[];
    const absent = required.filter((key) => args[key] === undefined || args[key] === null || args[key] === '');
    if (absent.length > 0) return rpcResult(id, errorResult(`missing required argument: ${absent.join(', ')}`));
    try {
      return rpcResult(id, callTool(name, args));
    } catch (err) {
      return rpcResult(id, errorResult(err instanceof Error ? err.message : 'tool failed'));
    }
  }

  if (isNotification) return null;
  return rpcError(id, -32601, `Method not found: ${method}`);
}

export async function handleMcp(ctx: McpContext): Promise<void> {
  if (ctx.method !== 'POST') {
    sendJson(ctx.res, 405, { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Use POST for JSON-RPC' } });
    return;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(ctx.body.toString('utf8'));
  } catch {
    sendJson(ctx.res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
    return;
  }

  if (Array.isArray(parsed)) {
    const answers: unknown[] = [];
    for (const entry of parsed) {
      if (entry === null || typeof entry !== 'object') {
        answers.push(rpcError(null, -32600, 'Invalid Request'));
        continue;
      }
      const answer = handleMessage(entry as Record<string, unknown>, ctx.req);
      if (answer !== null) answers.push(answer);
    }
    if (answers.length === 0) {
      ctx.res.writeHead(202);
      ctx.res.end();
      return;
    }
    sendJson(ctx.res, 200, answers);
    return;
  }

  if (parsed === null || typeof parsed !== 'object') {
    sendJson(ctx.res, 400, { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } });
    return;
  }

  const message = parsed as Record<string, unknown>;
  const answer = handleMessage(message, ctx.req);
  if (answer === null) {
    ctx.res.writeHead(202);
    ctx.res.end();
    return;
  }
  if (message.method === 'initialize') {
    ctx.res.setHeader('mcp-session-id', `forge-session-${store.repos.size}`);
    ctx.res.setHeader('mcp-protocol-version', declaredVersion(message, ctx.req));
  }
  sendJson(ctx.res, 200, answer);
}
