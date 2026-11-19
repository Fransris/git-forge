// GraphQL v4: a small but real parser and executor for the documents gh and github-mcp-server send —
// aliases, variables, inline enums/lists/input objects, nested selections, `... on Type`, __type
// introspection, mutations, and a JSON array of operations in one request.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { store, type Issue, type Pull, type Repo } from './store.ts';
import { buildCommit, flattenTree, mergeBase, parseCommit, putObject, readObject } from './git.ts';
import { sendJson } from './rest.ts';

export interface GraphqlContext {
  req: IncomingMessage;
  res: ServerResponse;
  method: string;
  body: Buffer;
  token: string | null;
}

// ---------------------------------------------------------------- lexer

type Token = { kind: 'name' | 'int' | 'float' | 'string' | 'punct' | 'spread'; value: string };

function lex(text: string): Token[] {
  const tokens: Token[] = [];
  let at = 0;
  while (at < text.length) {
    const ch = text[at]!;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
      at += 1;
      continue;
    }
    if (ch === '#') {
      while (at < text.length && text[at] !== '\n') at += 1;
      continue;
    }
    if (text.startsWith('...', at)) {
      tokens.push({ kind: 'spread', value: '...' });
      at += 3;
      continue;
    }
    if ('{}()[]:$!=@|'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      at += 1;
      continue;
    }
    if (ch === '"') {
      let value = '';
      at += 1;
      while (at < text.length && text[at] !== '"') {
        if (text[at] === '\\') {
          value += text[at + 1] ?? '';
          at += 2;
          continue;
        }
        value += text[at];
        at += 1;
      }
      at += 1;
      tokens.push({ kind: 'string', value });
      continue;
    }
    if (/[0-9]/.test(ch) || (ch === '-' && /[0-9]/.test(text[at + 1] ?? ''))) {
      let value = ch;
      at += 1;
      let isFloat = false;
      while (at < text.length && /[0-9.eE+-]/.test(text[at]!)) {
        if ('.' === text[at] || 'e' === text[at] || 'E' === text[at]) isFloat = true;
        value += text[at];
        at += 1;
      }
      tokens.push({ kind: isFloat ? 'float' : 'int', value });
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      let value = '';
      while (at < text.length && /[A-Za-z0-9_]/.test(text[at]!)) {
        value += text[at];
        at += 1;
      }
      tokens.push({ kind: 'name', value });
      continue;
    }
    at += 1;
  }
  return tokens;
}

// ---------------------------------------------------------------- parser

interface Field {
  alias: string | null;
  name: string;
  args: Record<string, unknown>;
  selections: Selection[];
  typeCondition: string | null;
}

type Selection = Field;

interface Operation {
  kind: 'query' | 'mutation';
  name: string | null;
  variables: Record<string, unknown>;
  selections: Selection[];
}

class Parser {
  private at = 0;
  private readonly tokens: Token[];
  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token | undefined {
    return this.tokens[this.at];
  }
  private next(): Token | undefined {
    return this.tokens[this.at++];
  }
  private expectPunct(value: string): void {
    const token = this.next();
    if (token === undefined || token.value !== value) throw new Error(`expected ${value}`);
  }
  private expectName(): string {
    const token = this.next();
    if (token === undefined || token.kind !== 'name') throw new Error('expected a name');
    return token.value;
  }

  parseDocument(): Operation[] {
    const operations: Operation[] = [];
    while (this.at < this.tokens.length) operations.push(this.parseOperation());
    return operations;
  }

  private parseOperation(): Operation {
    let kind: 'query' | 'mutation' = 'query';
    let name: string | null = null;
    const token = this.peek();
    if (token !== undefined && token.kind === 'name' && (token.value === 'query' || token.value === 'mutation')) {
      kind = this.next()!.value as 'query' | 'mutation';
      const nextToken = this.peek();
      if (nextToken !== undefined && nextToken.kind === 'name') name = this.next()!.value;
    }
    const variables: Record<string, unknown> = {};
    if (this.peek()?.value === '(') {
      this.next();
      while (this.peek() !== undefined && this.peek()!.value !== ')') {
        this.expectPunct('$');
        const varName = this.expectName();
        this.expectPunct(':');
        let depth = 0;
        while (this.peek() !== undefined) {
          const t = this.peek()!;
          if (t.value === '[') depth += 1;
          else if (t.value === ']') depth -= 1;
          else if ((t.value === '=' || t.value === ')') && depth === 0) break;
          this.next();
        }
        let defaultValue: unknown = null;
        if (this.peek()?.value === '=') {
          this.next();
          defaultValue = this.parseValue(variables);
        }
        variables[varName] = defaultValue;
      }
      this.expectPunct(')');
    }
    const selections = this.parseSelectionSet();
    return { kind, name, variables, selections };
  }

  private parseSelectionSet(): Selection[] {
    this.expectPunct('{');
    const selections: Selection[] = [];
    while (this.peek() !== undefined && this.peek()!.value !== '}') selections.push(this.parseSelection());
    this.expectPunct('}');
    return selections;
  }

  private parseSelection(): Selection {
    if (this.peek()?.kind === 'spread') {
      this.next();
      if (this.peek()?.value === 'on') {
        this.next();
        const typeCondition = this.expectName();
        const selections = this.parseSelectionSet();
        return { alias: null, name: '__inline', args: {}, selections, typeCondition };
      }
      if (this.peek()?.kind === 'name') this.next();
      return { alias: null, name: '__skip', args: {}, selections: [], typeCondition: null };
    }
    let name = this.expectName();
    let alias: string | null = null;
    if (this.peek()?.value === ':') {
      this.next();
      alias = name;
      name = this.expectName();
    }
    const args: Record<string, unknown> = {};
    if (this.peek()?.value === '(') {
      this.next();
      while (this.peek() !== undefined && this.peek()!.value !== ')') {
        const argName = this.expectName();
        this.expectPunct(':');
        args[argName] = this.parseValue({});
      }
      this.expectPunct(')');
    }
    let selections: Selection[] = [];
    if (this.peek()?.value === '{') selections = this.parseSelectionSet();
    return { alias, name, args, selections, typeCondition: null };
  }

  private parseValue(variables: Record<string, unknown>): unknown {
    const token = this.next();
    if (token === undefined) return null;
    if (token.value === '$') {
      const varName = this.expectName();
      return { __var: varName, fallback: variables[varName] };
    }
    if (token.value === '[') {
      const list: unknown[] = [];
      while (this.peek() !== undefined && this.peek()!.value !== ']') list.push(this.parseValue(variables));
      this.expectPunct(']');
      return list;
    }
    if (token.value === '{') {
      const object: Record<string, unknown> = {};
      while (this.peek() !== undefined && this.peek()!.value !== '}') {
        const key = this.expectName();
        this.expectPunct(':');
        object[key] = this.parseValue(variables);
      }
      this.expectPunct('}');
      return object;
    }
    if (token.kind === 'int') return Number.parseInt(token.value, 10);
    if (token.kind === 'float') return Number.parseFloat(token.value);
    if (token.kind === 'string') return token.value;
    if (token.value === 'null') return null;
    if (token.value === 'true') return true;
    if (token.value === 'false') return false;
    return token.value;
  }
}

/** Substitute `{__var}` markers with the request's variables, recursively. */
function bindVariables(value: unknown, variables: Record<string, unknown>): unknown {
  if (Array.isArray(value)) return value.map((entry) => bindVariables(entry, variables));
  if (value !== null && typeof value === 'object') {
    const record = value as Record<string, unknown>;
    if (typeof record.__var === 'string') {
      const name = record.__var;
      return name in variables ? variables[name] : (record.fallback ?? null);
    }
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(record)) out[k] = bindVariables(v, variables);
    return out;
  }
  return value;
}

// ---------------------------------------------------------------- entities

interface Entity {
  __type: string;
  value: unknown;
}

const entity = (type: string, value: unknown): Entity => ({ __type: type, value });

function globalId(type: string, id: number): string {
  return Buffer.from(`${type}:${id}`).toString('base64');
}

function parseGlobalId(id: string): { type: string; id: number } | null {
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const at = text.lastIndexOf(':');
    if (at < 0) return null;
    const type = text.slice(0, at);
    const number = Number.parseInt(text.slice(at + 1), 10);
    if (!Number.isFinite(number)) return null;
    return { type, id: number };
  } catch {
    return null;
  }
}

function issueById(id: number): { repo: Repo; issue: Issue } | null {
  for (const repo of store.repos.values()) {
    const issue = repo.issues.find((i) => i.id === id);
    if (issue !== undefined) return { repo, issue };
  }
  return null;
}

function pullById(id: number): { repo: Repo; pull: Pull } | null {
  for (const repo of store.repos.values()) {
    const pull = repo.pulls.find((p) => p.id === id);
    if (pull !== undefined) return { repo, pull };
  }
  return null;
}

function repoById(id: number): Repo | null {
  for (const repo of store.repos.values()) if (repo.id === id) return repo;
  return null;
}

const SCHEMA: Record<string, { fields: string[]; enumValues?: string[] }> = {
  Issue: { fields: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'closed', 'author', 'labels', 'comments'] },
  PullRequest: { fields: ['id', 'number', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files', 'title', 'body', 'createdAt', 'mergeCommit'] },
  Repository: { fields: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'] },
  User: { fields: ['login', 'id', 'name'] },
  SearchType: { fields: [], enumValues: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'] },
  IssueState: { fields: [], enumValues: ['OPEN', 'CLOSED'] },
  PullRequestState: { fields: [], enumValues: ['OPEN', 'CLOSED', 'MERGED'] },
  MergeableState: { fields: [], enumValues: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'] },
  PullRequestMergeMethod: { fields: [], enumValues: ['MERGE', 'SQUASH', 'REBASE'] },
  RepositoryPermission: { fields: [], enumValues: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'] },
  IssueOrderField: { fields: [], enumValues: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'] },
  OrderDirection: { fields: [], enumValues: ['ASC', 'DESC'] },
};

// ---------------------------------------------------------------- resolvers

function repoFiles(repo: Repo, ref: string): Map<string, { mode: string; sha: string }> {
  const tip = repo.refs.get(`refs/heads/${ref}`) ?? repo.refs.get(ref) ?? null;
  if (tip === null) return new Map();
  const object = readObject(repo, tip);
  if (object === null || object.type !== 'commit') return new Map();
  return flattenTree(repo, parseCommit(object.data).tree);
}

function pullFiles(repo: Repo, pull: Pull): Array<{ path: string }> {
  const before = repoFiles(repo, pull.base.ref);
  const after = repoFiles(repo, pull.head.ref);
  const out: Array<{ path: string }> = [];
  for (const [path, value] of after) if (before.get(path)?.sha !== value.sha) out.push({ path });
  for (const [path] of before) if (!after.has(path)) out.push({ path });
  return out.sort((a, b) => (a.path < b.path ? -1 : 1));
}

function pullMergeable(repo: Repo, pull: Pull): boolean {
  const baseTip = repo.refs.get(`refs/heads/${pull.base.ref}`) ?? null;
  const headTip = repo.refs.get(`refs/heads/${pull.head.ref}`) ?? null;
  if (baseTip === null || headTip === null) return false;
  const common = mergeBase(repo, baseTip, headTip);
  if (common === null) return false;
  const changed = (from: string, to: string): Set<string> => {
    const beforeObject = readObject(repo, from);
    const afterObject = readObject(repo, to);
    if (beforeObject === null || afterObject === null) return new Set();
    const before = flattenTree(repo, parseCommit(beforeObject.data).tree);
    const after = flattenTree(repo, parseCommit(afterObject.data).tree);
    const set = new Set<string>();
    for (const [p, v] of after) if (before.get(p)?.sha !== v.sha) set.add(p);
    for (const [p] of before) if (!after.has(p)) set.add(p);
    return set;
  };
  const onBase = changed(common, baseTip);
  const onHead = changed(common, headTip);
  return ![...onHead].some((p) => onBase.has(p));
}

const issueCursor = (index: number): string => Buffer.from(`cursor:${index}`).toString('base64');

function cursorIndex(cursor: string): number {
  try {
    const value = Number.parseInt(Buffer.from(cursor, 'base64').toString('utf8').split(':')[1] ?? '', 10);
    return Number.isFinite(value) ? value : 0;
  } catch {
    return 0;
  }
}

class FieldError extends Error {}

function resolveRoot(field: string, args: Record<string, unknown>): unknown {
  if (field === 'viewer') return entity('User', store.user());
  if (field === 'repository') {
    const owner = String(args.owner ?? '');
    const name = String(args.name ?? '');
    store.learnOwner(owner);
    const repo = store.find(owner, name);
    return repo === null ? null : entity('Repository', repo);
  }
  if (field === '__type') {
    const name = String(args.name ?? '');
    const entry = SCHEMA[name];
    return entry === undefined ? null : entity('__Type', { name, ...entry });
  }
  if (field === 'node') {
    const parsed = parseGlobalId(String(args.id ?? ''));
    if (parsed === null) return null;
    if (parsed.type === 'Repository') {
      const repo = repoById(parsed.id);
      return repo === null ? null : entity('Repository', repo);
    }
    if (parsed.type === 'Issue') {
      const found = issueById(parsed.id);
      return found === null ? null : entity('Issue', found);
    }
    if (parsed.type === 'PullRequest') {
      const found = pullById(parsed.id);
      return found === null ? null : entity('PullRequest', found);
    }
    return null;
  }
  throw new FieldError(`Cannot query field "${field}" on type "Query"`);
}

function resolveMutation(field: string, args: Record<string, unknown>): unknown {
  const input = (args.input ?? {}) as Record<string, unknown>;

  if (field === 'createIssue') {
    const parsed = parseGlobalId(String(input.repositoryId ?? ''));
    const repo = parsed === null ? null : repoById(parsed.id);
    if (repo === null) throw new FieldError('Could not resolve to a Repository with that id');
    const now = store.clock.tick();
    const issue: Issue = { id: store.newIssueId(), number: repo.next_issue++, title: String(input.title ?? ''), body: String(input.body ?? ''), state: 'open', labels: [], created_at: now, updated_at: now, closed_at: null, comments: [] };
    repo.issues.push(issue);
    return { issue: entity('Issue', { repo, issue }) };
  }

  if (field === 'closeIssue') {
    const parsed = parseGlobalId(String(input.issueId ?? ''));
    const found = parsed === null ? null : issueById(parsed.id);
    if (found === null) throw new FieldError('Could not resolve to an Issue with that id');
    found.issue.state = 'closed';
    found.issue.closed_at = store.clock.tick();
    found.issue.updated_at = found.issue.closed_at;
    return { issue: entity('Issue', found) };
  }

  if (field === 'createPullRequest') {
    const parsed = parseGlobalId(String(input.repositoryId ?? ''));
    const repo = parsed === null ? null : repoById(parsed.id);
    if (repo === null) throw new FieldError('Could not resolve to a Repository with that id');
    const headRef = String(input.headRefName ?? '');
    const baseRef = String(input.baseRefName ?? repo.default_branch);
    const headTip = repo.refs.get(`refs/heads/${headRef}`) ?? null;
    const baseTip = repo.refs.get(`refs/heads/${baseRef}`) ?? null;
    if (headTip === null || baseTip === null) throw new FieldError('Could not resolve to a branch with that name');
    const now = store.clock.tick();
    const pull: Pull = {
      id: store.newPullId(),
      number: repo.next_pull++,
      title: String(input.title ?? ''),
      body: String(input.body ?? ''),
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
    return { pullRequest: entity('PullRequest', { repo, pull }) };
  }

  if (field === 'mergePullRequest') {
    const parsed = parseGlobalId(String(input.pullRequestId ?? ''));
    const found = parsed === null ? null : pullById(parsed.id);
    if (found === null) throw new FieldError('Could not resolve to a PullRequest with that id');
    const { repo, pull } = found;
    if (!pull.merged && pullMergeable(repo, pull)) {
      const baseTip = repo.refs.get(`refs/heads/${pull.base.ref}`) ?? null;
      const headTip = repo.refs.get(`refs/heads/${pull.head.ref}`) ?? null;
      const headObject = headTip === null ? null : readObject(repo, headTip);
      if (baseTip !== null && headObject !== null) {
        const tree = parseCommit(headObject.data).tree;
        const time = store.clock.tick();
        const author = { name: repo.owner, email: `${repo.owner}@example.invalid` };
        const sha = putObject(repo, 'commit', buildCommit({ tree, parents: [baseTip], message: `Merge pull request #${pull.number} from ${pull.head.ref}\n\n${pull.title}`, author, time }));
        repo.refs.set(`refs/heads/${pull.base.ref}`, sha);
        repo.packCache.clear();
    repo.bodyCache.clear();
        pull.merged = true;
        pull.state = 'closed';
        pull.merged_at = time;
        pull.closed_at = time;
        pull.merge_commit_sha = sha;
        pull.updated_at = time;
      }
    }
    return { pullRequest: entity('PullRequest', { repo, pull }) };
  }

  if (field === 'addComment') {
    const parsed = parseGlobalId(String(input.subjectId ?? ''));
    const found = parsed === null ? null : issueById(parsed.id);
    if (found === null) throw new FieldError('Could not resolve to an Issue with that id');
    const now = store.clock.tick();
    const comment = { id: store.newCommentId(), body: String(input.body ?? ''), created_at: now, updated_at: now };
    found.issue.comments.push(comment);
    found.issue.updated_at = now;
    return { commentEdge: { node: { id: globalId('IssueComment', comment.id), body: comment.body } } };
  }

  throw new FieldError(`Cannot query field "${field}" on type "Mutation"`);
}

function resolveField(parent: Entity, field: string, args: Record<string, unknown>): unknown {
  const type = parent.__type;

  if (type === '__Type') {
    const info = parent.value as { name: string; fields: string[]; enumValues?: string[] };
    if (field === 'name') return info.name;
    if (field === 'fields') return info.fields.map((name) => entity('__Field', { name }));
    if (field === 'enumValues') return (info.enumValues ?? []).map((name) => entity('__EnumValue', { name }));
    if (field === 'kind') return info.enumValues !== undefined ? 'ENUM' : 'OBJECT';
    throw new FieldError(`Cannot query field "${field}" on type "__Type"`);
  }
  if (type === '__Field' || type === '__EnumValue') {
    if (field === 'name') return (parent.value as { name: string }).name;
    if (field === 'type') return entity('__TypeRef', { name: 'String', kind: 'SCALAR' });
    if (field === 'isDeprecated') return false;
    throw new FieldError(`Cannot query field "${field}" on type "${type}"`);
  }
  if (type === '__TypeRef') {
    const ref = parent.value as { name: string; kind: string };
    if (field === 'name') return ref.name;
    if (field === 'kind') return ref.kind;
    return null;
  }

  if (type === 'User') {
    const user = parent.value as { login: string; id?: number; name?: string };
    if (field === 'login') return user.login;
    if (field === 'id') return globalId('User', user.id ?? 1);
    if (field === 'name') return user.name ?? user.login;
    throw new FieldError(`Cannot query field "${field}" on type "User"`);
  }

  if (type === 'Repository') {
    const repo = parent.value as Repo;
    if (field === 'id') return globalId('Repository', repo.id);
    if (field === 'databaseId') return repo.id;
    if (field === 'name') return repo.name;
    if (field === 'nameWithOwner') return `${repo.owner}/${repo.name}`;
    if (field === 'owner') return entity('User', { login: repo.owner });
    if (field === 'defaultBranchRef') return repo.refs.has(`refs/heads/${repo.default_branch}`) ? entity('Ref', { name: repo.default_branch }) : null;
    if (field === 'viewerPermission') return 'ADMIN';
    if (field === 'hasIssuesEnabled') return true;
    if (field === 'issue') {
      const number = Number(args.number ?? 0);
      const issue = repo.issues.find((i) => i.number === number);
      return issue === undefined ? null : entity('Issue', { repo, issue });
    }
    if (field === 'issues') {
      const states = Array.isArray(args.states) ? (args.states as unknown[]).map((s) => String(s).toLowerCase()) : null;
      const orderBy = (args.orderBy ?? {}) as { field?: string; direction?: string };
      const direction = String(orderBy.direction ?? 'DESC').toUpperCase();
      let list = [...repo.issues];
      if (states !== null && states.length > 0) list = list.filter((i) => states.includes(i.state));
      const key = (i: Issue): number => (String(orderBy.field ?? 'CREATED_AT').toUpperCase() === 'UPDATED_AT' ? Date.parse(i.updated_at) : Date.parse(i.created_at));
      list.sort((a, b) => (key(a) - key(b)) * (direction === 'ASC' ? 1 : -1) || (a.number - b.number) * (direction === 'ASC' ? 1 : -1));
      const first = args.first === undefined || args.first === null ? list.length : Number(args.first);
      const after = typeof args.after === 'string' ? cursorIndex(args.after) : 0;
      const page = list.slice(after, after + first);
      const end = after + page.length;
      return entity('IssueConnection', {
        nodes: page.map((issue) => entity('Issue', { repo, issue })),
        pageInfo: { hasNextPage: end < list.length, endCursor: issueCursor(end), hasPreviousPage: after > 0, startCursor: issueCursor(after) },
        totalCount: list.length,
      });
    }
    if (field === 'pullRequests') {
      const list = [...repo.pulls].sort((a, b) => b.number - a.number);
      const first = args.first === undefined || args.first === null ? list.length : Number(args.first);
      const page = list.slice(0, first);
      return entity('PullRequestConnection', { nodes: page.map((pull) => entity('PullRequest', { repo, pull })), totalCount: list.length, pageInfo: { hasNextPage: page.length < list.length, endCursor: issueCursor(page.length), hasPreviousPage: false } });
    }
    if (field === 'pullRequest') {
      const number = Number(args.number ?? 0);
      const pull = repo.pulls.find((p) => p.number === number);
      return pull === undefined ? null : entity('PullRequest', { repo, pull });
    }
    throw new FieldError(`Cannot query field "${field}" on type "Repository"`);
  }

  if (type === 'Ref') {
    const ref = parent.value as { name: string };
    if (field === 'name') return ref.name;
    if (field === 'target') return null;
    throw new FieldError(`Cannot query field "${field}" on type "Ref"`);
  }

  if (type === 'Issue') {
    const { issue } = parent.value as { repo: Repo; issue: Issue };
    if (field === 'id') return globalId('Issue', issue.id);
    if (field === 'number') return issue.number;
    if (field === 'title') return issue.title;
    if (field === 'state') return issue.state === 'open' ? 'OPEN' : 'CLOSED';
    if (field === 'body') return issue.body;
    if (field === 'createdAt') return issue.created_at;
    if (field === 'updatedAt') return issue.updated_at;
    if (field === 'closed') return issue.state === 'closed';
    throw new FieldError(`Cannot query field "${field}" on type "Issue"`);
  }

  if (type === 'PullRequest') {
    const { repo, pull } = parent.value as { repo: Repo; pull: Pull };
    if (field === 'id') return globalId('PullRequest', pull.id);
    if (field === 'number') return pull.number;
    if (field === 'title') return pull.title;
    if (field === 'body') return pull.body;
    if (field === 'state') return pull.merged ? 'MERGED' : pull.state === 'open' ? 'OPEN' : 'CLOSED';
    if (field === 'merged') return pull.merged;
    if (field === 'headRefName') return pull.head.ref;
    if (field === 'baseRefName') return pull.base.ref;
    if (field === 'mergeable') return pullMergeable(repo, pull) ? 'MERGEABLE' : 'CONFLICTING';
    if (field === 'files') {
      const files = pullFiles(repo, pull);
      const first = args.first === undefined || args.first === null ? files.length : Number(args.first);
      const page = files.slice(0, first);
      return entity('PullRequestChangedFileConnection', { nodes: page.map((f) => entity('PullRequestChangedFile', f)), totalCount: files.length, pageInfo: { hasNextPage: page.length < files.length, endCursor: issueCursor(page.length), hasPreviousPage: false } });
    }
    throw new FieldError(`Cannot query field "${field}" on type "PullRequest"`);
  }

  if (type === 'PullRequestChangedFile') {
    const file = parent.value as { path: string };
    if (field === 'path') return file.path;
    throw new FieldError(`Cannot query field "${field}" on type "PullRequestChangedFile"`);
  }

  if (type.endsWith('Connection')) {
    const connection = parent.value as { nodes: Entity[]; pageInfo: Record<string, unknown>; totalCount: number };
    if (field === 'nodes') return connection.nodes;
    if (field === 'pageInfo') return entity('PageInfo', connection.pageInfo);
    if (field === 'totalCount') return connection.totalCount;
    if (field === 'edges') return connection.nodes.map((node, index) => entity('Edge', { node, cursor: issueCursor(index) }));
    throw new FieldError(`Cannot query field "${field}" on type "${type}"`);
  }

  if (type === 'Edge') {
    const edge = parent.value as { node: Entity; cursor: string };
    if (field === 'node') return edge.node;
    if (field === 'cursor') return edge.cursor;
    throw new FieldError(`Cannot query field "${field}" on type "Edge"`);
  }

  if (type === 'PageInfo') {
    const info = parent.value as Record<string, unknown>;
    if (field in info) return info[field];
    throw new FieldError(`Cannot query field "${field}" on type "PageInfo"`);
  }

  if (type === 'MutationPayload') {
    const payload = parent.value as Record<string, unknown>;
    if (field in payload) return payload[field];
    throw new FieldError(`Cannot query field "${field}" on type "MutationPayload"`);
  }

  if (type === 'CommentEdge') {
    const edge = parent.value as { node: { id: string; body: string } };
    if (field === 'node') return entity('Comment', edge.node);
    throw new FieldError(`Cannot query field "${field}" on type "CommentEdge"`);
  }

  if (type === 'Comment') {
    const comment = parent.value as { id: string; body: string };
    if (field === 'id') return comment.id;
    if (field === 'body') return comment.body;
    throw new FieldError(`Cannot query field "${field}" on type "Comment"`);
  }

  throw new FieldError(`Cannot query field "${field}" on type "${type}"`);
}

// ---------------------------------------------------------------- execution

function executeSelections(parent: Entity, selections: Selection[], path: string[], errors: Array<Record<string, unknown>>, vars: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    const key = selection.alias ?? selection.name;
    if (selection.name === '__skip') continue;
    if (selection.name === '__inline') {
      Object.assign(out, executeSelections(parent, selection.selections, path, errors, vars));
      continue;
    }
    if (selection.name === '__typename') {
      out[key] = parent.__type;
      continue;
    }
    let value: unknown;
    try {
      // Every selection's arguments bind against the request's variables, nested ones included.
      value = resolveField(parent, selection.name, bindVariables(selection.args, vars) as Record<string, unknown>);
    } catch (err) {
      errors.push({ message: err instanceof Error ? err.message : 'field failed', path: [...path, key] });
      continue;
    }
    out[key] = project(value, selection.selections, [...path, key], errors, vars);
  }
  return out;
}

function project(value: unknown, selections: Selection[], path: string[], errors: Array<Record<string, unknown>>, vars: Record<string, unknown>): unknown {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) return value.map((entry) => project(entry, selections, path, errors, vars));
  if (typeof value === 'object') {
    const maybeEntity = value as Partial<Entity>;
    if (typeof maybeEntity.__type === 'string' && 'value' in maybeEntity) {
      if (selections.length === 0) return null;
      return executeSelections(maybeEntity as Entity, selections, path, errors, vars);
    }
    if (selections.length === 0) return value;
    const out: Record<string, unknown> = {};
    const record = value as Record<string, unknown>;
    for (const selection of selections) {
      const key = selection.alias ?? selection.name;
      if (!(selection.name in record)) {
        errors.push({ message: `Cannot query field "${selection.name}"`, path: [...path, key] });
        continue;
      }
      out[key] = project(record[selection.name], selection.selections, [...path, key], errors, vars);
    }
    return out;
  }
  return value;
}

function runOperation(operation: Operation, variables: Record<string, unknown>): Record<string, unknown> {
  const errors: Array<Record<string, unknown>> = [];
  const bound = { ...operation.variables, ...variables };
  const data: Record<string, unknown> = {};

  for (const selection of operation.selections) {
    const key = selection.alias ?? selection.name;
    const args = bindVariables(selection.args, bound) as Record<string, unknown>;
    try {
      if (operation.kind === 'mutation') {
        const payload = resolveMutation(selection.name, args);
        data[key] = project(entity('MutationPayload', payload), selection.selections, [key], errors, bound);
      } else {
        data[key] = project(resolveRoot(selection.name, args), selection.selections, [key], errors, bound);
      }
    } catch (err) {
      errors.push({ message: err instanceof Error ? err.message : 'field failed', path: [key] });
      data[key] = null;
    }
  }
  return errors.length > 0 ? { data, errors } : { data };
}

export async function handleGraphql(ctx: GraphqlContext): Promise<void> {
  if (ctx.method !== 'POST') {
    sendJson(ctx.res, 405, { message: 'Use POST' });
    return;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(ctx.body.toString('utf8'));
  } catch {
    sendJson(ctx.res, 400, { errors: [{ message: 'Problems parsing JSON' }] });
    return;
  }

  const runOne = (request: unknown): Record<string, unknown> => {
    if (request === null || typeof request !== 'object') return { data: null, errors: [{ message: 'Invalid request' }] };
    const record = request as { query?: unknown; variables?: unknown };
    if (typeof record.query !== 'string') return { data: null, errors: [{ message: 'No query' }] };
    const variables = (record.variables !== null && typeof record.variables === 'object' ? record.variables : {}) as Record<string, unknown>;
    try {
      const operations = new Parser(lex(record.query)).parseDocument();
      if (operations.length === 0) return { data: null, errors: [{ message: 'No operation' }] };
      return runOperation(operations[0]!, variables);
    } catch (err) {
      return { data: null, errors: [{ message: err instanceof Error ? err.message : 'the document could not be parsed' }] };
    }
  };

  if (Array.isArray(parsed)) {
    sendJson(ctx.res, 200, parsed.map((entry) => runOne(entry)));
    return;
  }
  sendJson(ctx.res, 200, runOne(parsed));
}
