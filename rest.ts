// GitHub REST v3: repositories, branches, commits, contents, issues, comments, labels and pull
// requests, over the forge's store. Shapes follow api.github.com closely enough that gh reads them.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { Conflict, store, type Issue, type Label, type Pull, type Repo } from './store.ts';
import {
  buildCommit,
  flattenTree,
  mergeBase,
  parseCommit,
  parseTree,
  putObject,
  readObject,
  writeTreeFromPaths,
} from './git.ts';

export interface RestContext {
  req: IncomingMessage;
  res: ServerResponse;
  method: string;
  path: string; // the path after /api/v3, e.g. /repos/o/r
  query: URLSearchParams;
  body: Buffer;
  json: () => unknown;
  token: string | null;
}

const JSON_TYPE = 'application/json; charset=utf-8';
let requestCounter = 0;

export function sendJson(res: ServerResponse, status: number, body: unknown, extra: Record<string, string> = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  requestCounter += 1;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': `FORGE-${requestCounter}`,
    ...extra,
  });
  res.end(text);
}

export function sendEmpty(res: ServerResponse, status: number, extra: Record<string, string> = {}): void {
  requestCounter += 1;
  res.writeHead(status, { 'x-github-request-id': `FORGE-${requestCounter}`, ...extra });
  res.end();
}

function fail(res: ServerResponse, status: number, message: string): void {
  sendJson(res, status, { message });
}

function baseUrl(req: IncomingMessage): string {
  const host = String(req.headers.host ?? '127.0.0.1');
  return `http://${host}`;
}

// ---------------------------------------------------------------- shapes

function userShape(login: string): Record<string, unknown> {
  return {
    login,
    id: 1,
    node_id: `U_${login}`,
    avatar_url: '',
    gravatar_id: '',
    url: `/api/v3/users/${login}`,
    html_url: '',
    type: 'User',
    site_admin: false,
  };
}

function labelShape(label: Label): Record<string, unknown> {
  return { id: label.id, node_id: `LA_${label.id}`, name: label.name, color: label.color, default: false, description: label.description };
}

function issueShape(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: `I_${issue.id}`,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    locked: false,
    user: userShape(repo.owner),
    labels: issue.labels.map(labelShape),
    assignee: null,
    assignees: [],
    milestone: null,
    comments: issue.comments.length,
    created_at: issue.created_at,
    updated_at: issue.updated_at,
    closed_at: issue.closed_at,
    author_association: 'OWNER',
    html_url: '',
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    repository_url: `/api/v3/repos/${repo.owner}/${repo.name}`,
  };
}

function pullShape(repo: Repo, pull: Pull): Record<string, unknown> {
  const ref = (side: { ref: string; sha: string }): Record<string, unknown> => ({
    label: `${repo.owner}:${side.ref}`,
    ref: side.ref,
    sha: side.sha,
    user: userShape(repo.owner),
    repo: repoShape(repo),
  });
  return {
    id: pull.id,
    node_id: `PR_${pull.id}`,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    locked: false,
    user: userShape(repo.owner),
    head: ref(pull.head),
    base: ref(pull.base),
    merged: pull.merged,
    merged_at: pull.merged_at,
    merge_commit_sha: pull.merge_commit_sha,
    mergeable: pull.mergeable,
    mergeable_state: pull.mergeable ? 'clean' : 'dirty',
    draft: false,
    created_at: pull.created_at,
    updated_at: pull.updated_at,
    closed_at: pull.closed_at,
    commits: 1,
    additions: pull.files.reduce((a, f) => a + f.additions, 0),
    deletions: pull.files.reduce((a, f) => a + f.deletions, 0),
    changed_files: pull.files.length,
    html_url: '',
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
  };
}

function repoShape(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: `R_${repo.id}`,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    private: repo.private,
    owner: userShape(repo.owner),
    description: repo.description,
    fork: false,
    default_branch: repo.default_branch,
    created_at: repo.created_at,
    updated_at: repo.updated_at,
    pushed_at: repo.pushed_at,
    clone_url: `/${repo.owner}/${repo.name}.git`,
    html_url: '',
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    languages_url: `/api/v3/repos/${repo.owner}/${repo.name}/languages`,
    has_issues: true,
    open_issues_count: repo.issues.filter((i) => i.state === 'open').length,
    visibility: repo.private ? 'private' : 'public',
    permissions: { admin: true, push: true, pull: true },
  };
}

// ---------------------------------------------------------------- pagination

function paginate<T>(items: T[], query: URLSearchParams, req: IncomingMessage, res: ServerResponse, path: string): T[] {
  const perPageRaw = Number.parseInt(query.get('per_page') ?? '30', 10);
  const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(perPageRaw, 100) : 30;
  const pageRaw = Number.parseInt(query.get('page') ?? '1', 10);
  const page = Number.isFinite(pageRaw) && pageRaw > 0 ? pageRaw : 1;
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  const links: string[] = [];
  const origin = baseUrl(req);
  const makeUrl = (target: number): string => {
    const params = new URLSearchParams(query);
    params.set('per_page', String(perPage));
    params.set('page', String(target));
    return `${origin}/api/v3${path}?${params.toString()}`;
  };
  if (start + perPage < items.length) links.push(`<${makeUrl(page + 1)}>; rel="next"`);
  if (page > 1) links.push(`<${makeUrl(page - 1)}>; rel="prev"`);
  links.push(`<${makeUrl(1)}>; rel="first"`);
  if (links.length > 0) res.setHeader('link', links.join(', '));
  return slice;
}

// ---------------------------------------------------------------- the router

export function handleRest(ctx: RestContext): boolean {
  const { method, path, query, res, req } = ctx;
  const segments = path.split('/').filter((s) => s !== '');

  if (method === 'GET' && (path === '' || path === '/')) {
    sendJson(res, 200, {
      current_user_url: '/api/v3/user',
      repository_url: '/api/v3/repos/{owner}/{repo}',
      user_repositories_url: '/api/v3/user/repos',
      rate_limit_url: '/api/v3/rate_limit',
      graphql_url: '/api/graphql',
    }, { 'x-oauth-scopes': 'repo, read:org' });
    return true;
  }

  if (method === 'GET' && path === '/meta') {
    sendJson(res, 200, {
      installed_version: '3.0.0-forge',
      verifiable_password_authentication: true,
      hooks: [],
      git: [],
      ssh_key_fingerprints: {},
      ssh_keys: [],
      web: [],
      api: [],
      packages: [],
    });
    return true;
  }

  if (method === 'GET' && path === '/rate_limit') {
    sendJson(res, 200, { resources: {}, rate: { limit: 5000, remaining: 4999, reset: 0, used: 1 } });
    return true;
  }

  if (method === 'GET' && path === '/user') {
    const user = store.user();
    sendJson(res, 200, { ...userShape(user.login), name: user.name, email: user.email, public_repos: store.reposOf(user.login).length });
    return true;
  }

  if (path === '/user/repos') {
    if (method === 'GET') {
      const list = store.reposOf(store.login());
      const slice = paginate(list, query, req, res, '/user/repos');
      sendJson(res, 200, slice.map(repoShape));
      return true;
    }
    if (method === 'POST') {
      const body = ctx.json() as { name?: unknown; description?: unknown; private?: unknown };
      const name = typeof body.name === 'string' ? body.name : '';
      if (name === '') {
        sendJson(res, 422, { message: 'Repository creation failed.', errors: [{ resource: 'Repository', field: 'name', code: 'missing_field' }] });
        return true;
      }
      try {
        const repo = store.createRepo(name, typeof body.description === 'string' ? body.description : '', body.private === true);
        sendJson(res, 201, repoShape(repo));
      } catch (err) {
        if (err instanceof Conflict) {
          sendJson(res, 422, { message: 'Repository creation failed.', errors: [{ resource: 'Repository', field: 'name', code: 'custom', message: 'name already exists on this account' }] });
          return true;
        }
        throw err;
      }
      return true;
    }
  }

  if (segments[0] === 'users' && segments[1] !== undefined && segments.length === 2 && method === 'GET') {
    sendJson(res, 200, { ...userShape(segments[1]), name: segments[1], public_repos: 0 });
    return true;
  }

  if (segments[0] !== 'repos' || segments.length < 3) return false;
  const owner = segments[1]!;
  const name = segments[2]!;
  store.learnOwner(owner);
  const repo = store.find(owner, name);
  if (repo === null) {
    fail(res, 404, 'Not Found');
    return true;
  }
  const rest = segments.slice(3);

  // /repos/{o}/{r}
  if (rest.length === 0) {
    if (method === 'GET') {
      sendJson(res, 200, repoShape(repo));
      return true;
    }
    if (method === 'PATCH') {
      const body = ctx.json() as { description?: unknown; name?: unknown; private?: unknown; default_branch?: unknown };
      if (typeof body.description === 'string') repo.description = body.description;
      if (typeof body.name === 'string' && body.name !== '') {
        store.repos.delete(store.key(repo.owner, repo.name));
        repo.name = body.name;
        store.repos.set(store.key(repo.owner, repo.name), repo);
      }
      if (typeof body.private === 'boolean') repo.private = body.private;
      if (typeof body.default_branch === 'string') repo.default_branch = body.default_branch;
      repo.updated_at = store.clock.tick();
      sendJson(res, 200, repoShape(repo));
      return true;
    }
    if (method === 'DELETE') {
      store.deleteRepo(repo.owner, repo.name);
      sendEmpty(res, 204);
      return true;
    }
  }

  if (rest[0] === 'branches' && rest.length === 1 && method === 'GET') {
    const branches = [...repo.refs.entries()]
      .filter(([ref]) => ref.startsWith('refs/heads/'))
      .map(([ref, sha]) => ({ name: ref.slice('refs/heads/'.length), commit: { sha, url: `/api/v3/repos/${repo.owner}/${repo.name}/commits/${sha}` }, protected: false }))
      .sort((a, b) => (a.name < b.name ? -1 : 1));
    sendJson(res, 200, paginate(branches, query, req, res, `/repos/${repo.owner}/${repo.name}/branches`));
    return true;
  }

  if (rest[0] === 'git' && rest[1] === 'refs' && method === 'POST') {
    const body = ctx.json() as { ref?: unknown; sha?: unknown };
    const ref = typeof body.ref === 'string' ? body.ref : '';
    const sha = typeof body.sha === 'string' ? body.sha : '';
    if (ref === '' || sha === '' || readObject(repo, sha) === null) {
      sendJson(res, 422, { message: 'Reference update failed', errors: [{ resource: 'GitRef', code: 'custom' }] });
      return true;
    }
    const full = ref.startsWith('refs/') ? ref : `refs/heads/${ref}`;
    repo.refs.set(full, sha);
    repo.packCache.clear();
    repo.bodyCache.clear();
    sendJson(res, 201, { ref: full, node_id: `REF_${full}`, url: `/api/v3/repos/${repo.owner}/${repo.name}/git/refs`, object: { sha, type: 'commit', url: '' } });
    return true;
  }

  if (rest[0] === 'commits' && method === 'GET') {
    const branch = query.get('sha') ?? query.get('branch') ?? repo.default_branch;
    const ref = branch.includes('/') && branch.startsWith('refs/') ? branch : `refs/heads/${branch}`;
    const tip = repo.refs.get(ref) ?? repo.refs.get(`refs/heads/${branch}`) ?? null;
    const commits: unknown[] = [];
    if (tip !== null) {
      const seen = new Set<string>();
      const queue: string[] = [tip];
      while (queue.length > 0) {
        const id = queue.shift()!;
        if (seen.has(id)) continue;
        seen.add(id);
        const object = readObject(repo, id);
        if (object === null || object.type !== 'commit') continue;
        const info = parseCommit(object.data);
        commits.push({
          sha: id,
          node_id: `C_${id}`,
          commit: {
            message: info.message.replace(/\n+$/, ''),
            author: { name: info.authorName, email: info.authorEmail, date: info.time },
            committer: { name: info.authorName, email: info.authorEmail, date: info.time },
            tree: { sha: info.tree },
          },
          parents: info.parents.map((parent) => ({ sha: parent })),
          url: `/api/v3/repos/${repo.owner}/${repo.name}/commits/${id}`,
        });
        for (const parent of info.parents) queue.push(parent);
      }
    }
    sendJson(res, 200, paginate(commits, query, req, res, `/repos/${repo.owner}/${repo.name}/commits`));
    return true;
  }

  if (rest[0] === 'contents') {
    const filePath = rest.slice(1).join('/');
    return handleContents(ctx, repo, filePath);
  }

  if (rest[0] === 'labels') {
    if (method === 'GET' && rest.length === 1) {
      sendJson(res, 200, repo.labels.map(labelShape));
      return true;
    }
    if (method === 'POST' && rest.length === 1) {
      const body = ctx.json() as { name?: unknown; color?: unknown; description?: unknown };
      const labelName = typeof body.name === 'string' ? body.name : '';
      if (labelName === '') {
        sendJson(res, 422, { message: 'Validation Failed' });
        return true;
      }
      const existing = repo.labels.find((l) => l.name === labelName);
      if (existing !== undefined) {
        sendJson(res, 201, labelShape(existing));
        return true;
      }
      const label: Label = {
        id: store.newLabelId(),
        name: labelName,
        color: typeof body.color === 'string' ? body.color.replace('#', '') : 'ededed',
        description: typeof body.description === 'string' ? body.description : '',
      };
      repo.labels.push(label);
      sendJson(res, 201, labelShape(label));
      return true;
    }
  }

  if (rest[0] === 'issues') {
    return handleIssues(ctx, repo, rest.slice(1));
  }

  if (rest[0] === 'pulls') {
    return handlePulls(ctx, repo, rest.slice(1));
  }

  return false;
}

// ---------------------------------------------------------------- contents

function handleContents(ctx: RestContext, repo: Repo, filePath: string): boolean {
  const { method, res } = ctx;
  // ?ref= names the branch or commit to read; a write names its branch in the body.
  const refParam = ctx.query.get('ref');
  const branchForRead = (refParam !== null && refParam !== '' ? refParam : repo.default_branch).replace(/^refs\/heads\//, '');
  const tip = repo.refs.get(`refs/heads/${branchForRead}`) ?? null;
  const files = tip === null ? new Map<string, { mode: string; sha: string }>() : flattenTree(repo, parseCommit(readObject(repo, tip)!.data).tree);

  if (method === 'GET') {
    const clean = filePath.replace(/^\/+|\/+$/g, '');
    if (clean === '') {
      const entries = [...files.entries()]
        .filter(([p]) => !p.includes('/'))
        .map(([p, v]) => ({ type: 'file', name: p, path: p, sha: v.sha, size: readObject(repo, v.sha)?.data.length ?? 0, encoding: 'base64', content: '' }));
      const dirs = new Set<string>();
      for (const p of files.keys()) if (p.includes('/')) dirs.add(p.split('/')[0]!);
      for (const d of dirs) entries.push({ type: 'dir', name: d, path: d, sha: '', size: 0, encoding: 'base64', content: '' });
      sendJson(res, 200, entries);
      return true;
    }
    const entry = files.get(clean);
    if (entry !== undefined) {
      const object = readObject(repo, entry.sha);
      if (object !== null) {
        sendJson(res, 200, {
          type: 'file',
          encoding: 'base64',
          size: object.data.length,
          name: clean.split('/').pop() ?? clean,
          path: clean,
          content: object.data.toString('base64'),
          sha: entry.sha,
          url: `/api/v3/repos/${repo.owner}/${repo.name}/contents/${clean}`,
        });
        return true;
      }
    }
    // a directory?
    const prefix = `${clean}/`;
    const children = [...files.entries()].filter(([p]) => p.startsWith(prefix));
    if (children.length > 0) {
      const names = new Map<string, { type: 'file' | 'dir'; path: string; sha: string; size: number }>();
      for (const [p, v] of children) {
        const tail = p.slice(prefix.length);
        const slash = tail.indexOf('/');
        if (slash < 0) names.set(tail, { type: 'file', path: p, sha: v.sha, size: readObject(repo, v.sha)?.data.length ?? 0 });
        else {
          const dir = tail.slice(0, slash);
          if (!names.has(dir)) names.set(dir, { type: 'dir', path: `${clean}/${dir}`, sha: '', size: 0 });
        }
      }
      sendJson(res, 200, [...names.entries()].map(([n, v]) => ({ type: v.type, name: n, path: v.path, sha: v.sha, size: v.size, encoding: 'base64', content: '' })));
      return true;
    }
    fail(res, 404, 'Not Found');
    return true;
  }

  if (method === 'PUT') {
    const body = ctx.json() as { message?: unknown; content?: unknown; sha?: unknown; branch?: unknown };
    const clean = filePath.replace(/^\/+|\/+$/g, '');
    if (clean === '') {
      sendJson(res, 422, { message: 'Invalid path' });
      return true;
    }
    const encoded = typeof body.content === 'string' ? body.content : '';
    const data = Buffer.from(encoded.replace(/\n/g, ''), 'base64');
    const existing = files.get(clean);
    const suppliedSha = typeof body.sha === 'string' ? body.sha : null;
    if (suppliedSha !== null && existing !== undefined && existing.sha !== suppliedSha) {
      sendJson(res, 409, { message: `${clean} does not match ${suppliedSha}` });
      return true;
    }
    if (suppliedSha !== null && existing === undefined) {
      sendJson(res, 409, { message: `${clean} does not exist at ${suppliedSha}` });
      return true;
    }
    const writeBranch = (typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.default_branch).replace(/^refs\/heads\//, '');
    const writeTip = repo.refs.get(`refs/heads/${writeBranch}`) ?? null;
    const branchFiles = writeTip === null ? new Map<string, { mode: string; sha: string }>() : flattenTree(repo, parseCommit(readObject(repo, writeTip)!.data).tree);
    const branchExisting = branchFiles.get(clean);
    if (suppliedSha !== null && branchExisting !== undefined && branchExisting.sha !== suppliedSha) {
      sendJson(res, 409, { message: `${clean} does not match ${suppliedSha}` });
      return true;
    }
    const isCreate = branchExisting === undefined;
    const blob = putObject(repo, 'blob', data);
    branchFiles.set(clean, { mode: '100644', sha: blob });
    const commitSha = commitOnBranch(repo, writeBranch, branchFiles, typeof body.message === 'string' ? body.message : `Update ${clean}`, writeTip === null ? [] : [writeTip]);
    const content = {
      name: clean.split('/').pop() ?? clean,
      path: clean,
      sha: blob,
      size: data.length,
      encoding: 'base64',
      content: data.toString('base64'),
      url: `/api/v3/repos/${repo.owner}/${repo.name}/contents/${clean}`,
    };
    sendJson(res, isCreate ? 201 : 200, {
      content,
      commit: {
        sha: commitSha,
        node_id: `C_${commitSha}`,
        message: typeof body.message === 'string' ? body.message : `Update ${clean}`,
        url: `/api/v3/repos/${repo.owner}/${repo.name}/commits/${commitSha}`,
      },
    });
    return true;
  }

  if (method === 'DELETE') {
    const body = ctx.json() as { message?: unknown; sha?: unknown };
    const clean = filePath.replace(/^\/+|\/+$/g, '');
    const existing = files.get(clean);
    if (existing === undefined) {
      fail(res, 404, 'Not Found');
      return true;
    }
    const suppliedSha = typeof body.sha === 'string' ? body.sha : null;
    if (suppliedSha !== null && suppliedSha !== existing.sha) {
      sendJson(res, 409, { message: `${clean} does not match ${suppliedSha}` });
      return true;
    }
    const message = typeof body.message === 'string' ? body.message : `Delete ${clean}`;
    const deleteBranch = (typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.default_branch).replace(/^refs\/heads\//, '');
    const deleteTip = repo.refs.get(`refs/heads/${deleteBranch}`) ?? null;
    const deleteFiles = deleteTip === null ? new Map<string, { mode: string; sha: string }>() : flattenTree(repo, parseCommit(readObject(repo, deleteTip)!.data).tree);
    if (!deleteFiles.has(clean)) {
      fail(res, 404, 'Not Found');
      return true;
    }
    deleteFiles.delete(clean);
    const commitSha = commitOnBranch(repo, deleteBranch, deleteFiles, message, deleteTip === null ? [] : [deleteTip]);
    sendJson(res, 200, {
      content: null,
      commit: { sha: commitSha, node_id: `C_${commitSha}`, message, url: `/api/v3/repos/${repo.owner}/${repo.name}/commits/${commitSha}` },
    });
    return true;
  }
  return false;
}

function commitOnBranch(repo: Repo, branch: string, files: Map<string, { mode: string; sha: string }>, message: string, parents: string[]): string {
  const tree = writeTreeFromPaths(repo, files);
  const time = store.clock.tick();
  const author = { name: repo.owner, email: `${repo.owner}@example.invalid` };
  const commit = putObject(repo, 'commit', buildCommit({ tree, parents, message, author, time }));
  repo.refs.set(`refs/heads/${branch}`, commit);
  repo.packCache.clear();
    repo.bodyCache.clear();
  repo.pushed_at = time;
  return commit;
}

// ---------------------------------------------------------------- issues

function handleIssues(ctx: RestContext, repo: Repo, rest: string[]): boolean {
  const { method, res, query } = ctx;

  if (rest.length === 0) {
    if (method === 'GET') {
      const state = query.get('state') ?? 'open';
      const sort = query.get('sort') ?? 'created';
      const direction = query.get('direction') ?? 'desc';
      let list = [...repo.issues];
      if (state !== 'all') list = list.filter((i) => i.state === state);
      const key = (i: Issue): number => (sort === 'updated' ? Date.parse(i.updated_at) : Date.parse(i.created_at));
      list.sort((a, b) => (key(a) - key(b)) * (direction === 'asc' ? 1 : -1) || (a.number - b.number) * (direction === 'asc' ? 1 : -1));
      sendJson(res, 200, paginate(list, query, ctx.req, res, `/repos/${repo.owner}/${repo.name}/issues`).map((i) => issueShape(repo, i)));
      return true;
    }
    if (method === 'POST') {
      const body = ctx.json() as { title?: unknown; body?: unknown; labels?: unknown };
      const title = typeof body.title === 'string' ? body.title : '';
      if (title === '') {
        sendJson(res, 422, { message: 'Validation Failed', errors: [{ resource: 'Issue', field: 'title', code: 'missing_field' }] });
        return true;
      }
      const wanted = Array.isArray(body.labels) ? body.labels.filter((l): l is string => typeof l === 'string') : [];
      const labels: Label[] = [];
      for (const labelName of wanted) {
        const found = repo.labels.find((l) => l.name === labelName);
        if (found === undefined) {
          sendJson(res, 422, { message: 'Validation Failed', errors: [{ resource: 'Issue', field: 'labels', code: 'invalid', message: `Label does not exist: ${labelName}` }] });
          return true;
        }
        labels.push(found);
      }
      const now = store.clock.tick();
      const issue: Issue = {
        id: store.newIssueId(),
        number: repo.next_issue++,
        title,
        body: typeof body.body === 'string' ? body.body : '',
        state: 'open',
        labels,
        created_at: now,
        updated_at: now,
        closed_at: null,
        comments: [],
      };
      repo.issues.push(issue);
      sendJson(res, 201, issueShape(repo, issue));
      return true;
    }
  }

  const number = Number.parseInt(rest[0] ?? '', 10);
  if (!Number.isFinite(number)) return false;
  const issue = repo.issues.find((i) => i.number === number);
  if (issue === undefined) {
    fail(res, 404, 'Not Found');
    return true;
  }

  if (rest.length === 1) {
    if (method === 'GET') {
      sendJson(res, 200, issueShape(repo, issue));
      return true;
    }
    if (method === 'PATCH') {
      const body = ctx.json() as { title?: unknown; body?: unknown; state?: unknown; labels?: unknown };
      if (typeof body.title === 'string') issue.title = body.title;
      if (typeof body.body === 'string') issue.body = body.body;
      if (body.state === 'open' || body.state === 'closed') {
        issue.state = body.state;
        issue.closed_at = body.state === 'closed' ? store.clock.tick() : null;
      }
      if (Array.isArray(body.labels)) {
        const labels: Label[] = [];
        for (const labelName of body.labels.filter((l): l is string => typeof l === 'string')) {
          const found = repo.labels.find((l) => l.name === labelName);
          if (found !== undefined) labels.push(found);
        }
        issue.labels = labels;
      }
      issue.updated_at = store.clock.tick();
      sendJson(res, 200, issueShape(repo, issue));
      return true;
    }
  }

  if (rest[1] === 'comments') {
    if (method === 'GET') {
      sendJson(res, 200, issue.comments.map((c) => ({ id: c.id, node_id: `IC_${c.id}`, body: c.body, user: userShape(repo.owner), created_at: c.created_at, updated_at: c.updated_at, url: '' })));
      return true;
    }
    if (method === 'POST' && rest.length === 2) {
      const body = ctx.json() as { body?: unknown };
      const text = typeof body.body === 'string' ? body.body : '';
      if (text === '') {
        sendJson(res, 422, { message: 'Validation Failed' });
        return true;
      }
      const now = store.clock.tick();
      const comment = { id: store.newCommentId(), body: text, created_at: now, updated_at: now };
      issue.comments.push(comment);
      issue.updated_at = now;
      sendJson(res, 201, { id: comment.id, node_id: `IC_${comment.id}`, body: comment.body, user: userShape(repo.owner), created_at: now, updated_at: now, url: '' });
      return true;
    }
  }
  return false;
}

// ---------------------------------------------------------------- pulls

function diffFiles(repo: Repo, baseSha: string, headSha: string): Pull['files'] {
  const read = (sha: string): Map<string, { mode: string; sha: string }> => {
    const object = readObject(repo, sha);
    if (object === null || object.type !== 'commit') return new Map();
    return flattenTree(repo, parseCommit(object.data).tree);
  };
  const baseFiles = read(baseSha);
  const headFiles = read(headSha);
  const files: Pull['files'] = [];
  const paths = new Set([...baseFiles.keys(), ...headFiles.keys()]);
  for (const path of [...paths].sort()) {
    const before = baseFiles.get(path);
    const after = headFiles.get(path);
    if (before !== undefined && after !== undefined && before.sha === after.sha) continue;
    const beforeData = before === undefined ? '' : (readObject(repo, before.sha)?.data.toString('utf8') ?? '');
    const afterData = after === undefined ? '' : (readObject(repo, after.sha)?.data.toString('utf8') ?? '');
    const beforeLines = beforeData === '' ? [] : beforeData.split('\n');
    const afterLines = afterData === '' ? [] : afterData.split('\n');
    let additions = 0;
    let deletions = 0;
    const max = Math.max(beforeLines.length, afterLines.length);
    for (let i = 0; i < max; i += 1) {
      if (beforeLines[i] === afterLines[i]) continue;
      if (beforeLines[i] !== undefined) deletions += 1;
      if (afterLines[i] !== undefined) additions += 1;
    }
    files.push({
      filename: path,
      status: before === undefined ? 'added' : after === undefined ? 'removed' : 'modified',
      additions,
      deletions,
      changes: additions + deletions,
      sha: after?.sha ?? before?.sha ?? '',
      blob_url: '',
    });
  }
  return files;
}

function refreshPull(repo: Repo, pull: Pull): void {
  const baseTip = repo.refs.get(`refs/heads/${pull.base.ref}`) ?? null;
  const headTip = repo.refs.get(`refs/heads/${pull.head.ref}`) ?? null;
  pull.base.sha = baseTip ?? pull.base.sha;
  pull.head.sha = headTip ?? pull.head.sha;
  if (baseTip !== null && headTip !== null) {
    pull.files = diffFiles(repo, baseTip, headTip);
    const common = mergeBase(repo, baseTip, headTip);
    if (common === null) {
      pull.mergeable = false;
    } else {
      const changedOn = (from: string, to: string): Set<string> => {
        const before = flattenTree(repo, parseCommit(readObject(repo, from)!.data).tree);
        const after = flattenTree(repo, parseCommit(readObject(repo, to)!.data).tree);
        const changed = new Set<string>();
        for (const [p, v] of after) if (before.get(p)?.sha !== v.sha) changed.add(p);
        for (const [p] of before) if (!after.has(p)) changed.add(p);
        return changed;
      };
      const onBase = changedOn(common, baseTip);
      const onHead = changedOn(common, headTip);
      const overlap = [...onHead].filter((p) => onBase.has(p));
      pull.mergeable = overlap.length === 0;
    }
  }
}

function handlePulls(ctx: RestContext, repo: Repo, rest: string[]): boolean {
  const { method, res, query } = ctx;

  if (rest.length === 0) {
    if (method === 'GET') {
      const state = query.get('state') ?? 'open';
      let list = [...repo.pulls];
      if (state !== 'all') list = list.filter((p) => p.state === state);
      list.sort((a, b) => b.number - a.number);
      for (const pull of list) refreshPull(repo, pull);
      sendJson(res, 200, paginate(list, query, ctx.req, res, `/repos/${repo.owner}/${repo.name}/pulls`).map((p) => pullShape(repo, p)));
      return true;
    }
    if (method === 'POST') {
      const body = ctx.json() as { title?: unknown; body?: unknown; head?: unknown; base?: unknown };
      const headRef = typeof body.head === 'string' ? body.head : '';
      const baseRef = typeof body.base === 'string' ? body.base : repo.default_branch;
      const headTip = repo.refs.get(`refs/heads/${headRef}`) ?? null;
      const baseTip = repo.refs.get(`refs/heads/${baseRef}`) ?? null;
      if (headTip === null || baseTip === null) {
        sendJson(res, 422, { message: 'Validation Failed', errors: [{ resource: 'PullRequest', code: 'invalid', field: 'head' }] });
        return true;
      }
      const now = store.clock.tick();
      const pull: Pull = {
        id: store.newPullId(),
        number: repo.next_pull++,
        title: typeof body.title === 'string' ? body.title : '',
        body: typeof body.body === 'string' ? body.body : '',
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
      refreshPull(repo, pull);
      sendJson(res, 201, pullShape(repo, pull));
      return true;
    }
  }

  const number = Number.parseInt(rest[0] ?? '', 10);
  if (!Number.isFinite(number)) return false;
  const pull = repo.pulls.find((p) => p.number === number);
  if (pull === undefined) {
    fail(res, 404, 'Not Found');
    return true;
  }
  refreshPull(repo, pull);

  if (rest.length === 1 && method === 'GET') {
    sendJson(res, 200, pullShape(repo, pull));
    return true;
  }

  if (rest[1] === 'files' && method === 'GET') {
    sendJson(res, 200, pull.files.map((f) => ({ ...f, additions: f.additions, deletions: f.deletions, changes: f.changes, status: f.status, filename: f.filename, sha: f.sha, blob_url: '' })));
    return true;
  }

  if (rest[1] === 'merge' && method === 'PUT') {
    if (pull.merged) {
      sendJson(res, 405, { message: 'Pull Request is not mergeable' });
      return true;
    }
    if (!pull.mergeable) {
      sendJson(res, 405, { message: 'Pull Request is not mergeable' });
      return true;
    }
    const headTip = repo.refs.get(`refs/heads/${pull.head.ref}`) ?? null;
    const baseTip = repo.refs.get(`refs/heads/${pull.base.ref}`) ?? null;
    if (headTip === null || baseTip === null) {
      sendJson(res, 405, { message: 'Pull Request is not mergeable' });
      return true;
    }
    const headObject = readObject(repo, headTip);
    if (headObject === null) {
      sendJson(res, 405, { message: 'Pull Request is not mergeable' });
      return true;
    }
    const tree = parseCommit(headObject.data).tree;
    const time = store.clock.tick();
    const message = `Merge pull request #${pull.number} from ${pull.head.ref}\n\n${pull.title}`;
    const author = { name: repo.owner, email: `${repo.owner}@example.invalid` };
    const mergeSha = putObject(repo, 'commit', buildCommit({ tree, parents: [baseTip], message, author, time }));
    repo.refs.set(`refs/heads/${pull.base.ref}`, mergeSha);
    repo.packCache.clear();
    repo.bodyCache.clear();
    pull.state = 'closed';
    pull.merged = true;
    pull.merged_at = time;
    pull.closed_at = time;
    pull.merge_commit_sha = mergeSha;
    pull.updated_at = time;
    repo.pushed_at = time;
    sendJson(res, 200, { sha: mergeSha, merged: true, message: 'Pull Request successfully merged' });
    return true;
  }

  return false;
}
