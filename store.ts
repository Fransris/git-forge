// The forge's store: users, repositories, issues, pull requests, labels and the git object graph.
// Everything is deterministic — ids from counters, times from a fixed epoch plus a tick — so two runs
// of the same trial leave the same export, byte for byte.

export const EPOCH_MS = Date.UTC(2026, 0, 1, 0, 0, 0);

/** A deterministic clock: one tick per mutation, never a wall-clock read. */
export class Clock {
  private ticks = 0;
  tick(): string {
    this.ticks += 1;
    return new Date(EPOCH_MS + this.ticks * 1000).toISOString().replace('.000Z', 'Z');
  }
  now(): string {
    return new Date(EPOCH_MS + this.ticks * 1000).toISOString().replace('.000Z', 'Z');
  }
}

export interface User {
  id: number;
  login: string;
  name: string;
  email: string;
  type: 'User';
  site_admin: boolean;
}

export interface Label {
  id: number;
  name: string;
  color: string;
  description: string;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
  created_at: string;
  updated_at: string;
  closed_at: string | null;
  comments: Comment[];
}

export interface Comment {
  id: number;
  body: string;
  created_at: string;
  updated_at: string;
}

export interface PullFile {
  filename: string;
  status: 'added' | 'modified' | 'removed';
  additions: number;
  deletions: number;
  changes: number;
  sha: string;
  blob_url: string;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  head: { ref: string; sha: string };
  base: { ref: string; sha: string };
  created_at: string;
  updated_at: string;
  closed_at: string | null;
  merged: boolean;
  merged_at: string | null;
  merge_commit_sha: string | null;
  mergeable: boolean;
  files: PullFile[];
}

export interface Repo {
  id: number;
  name: string;
  owner: string;
  description: string;
  private: boolean;
  default_branch: string;
  created_at: string;
  updated_at: string;
  pushed_at: string;
  issues: Issue[];
  pulls: Pull[];
  labels: Label[];
  next_issue: number;
  next_pull: number;
  // git side
  objects: Map<string, GitObject>;
  refs: Map<string, string>;
  /** Packs already built for a fetch, keyed by the tips, depth and haves it was built from. */
  packCache: Map<string, Buffer>;
  /** Whole response bodies already framed for a fetch or an ls-refs, keyed the same way. */
  bodyCache: Map<string, Buffer>;
}

export interface GitObject {
  type: 'blob' | 'tree' | 'commit' | 'tag';
  data: Buffer;
}

export class Store {
  readonly clock = new Clock();
  readonly users = new Map<string, User>();
  readonly repos = new Map<string, Repo>(); // key: `${owner}/${name}` (lowercased)
  /** The login this forge serves. Learned from the first repository path the suite asks for. */
  viewer: string | null = null;
  private nextRepoId = 1000;
  private nextIssueId = 100_000;
  private nextCommentId = 200_000;
  private nextPullId = 300_000;
  private nextLabelId = 400_000;

  /**
   * Adopt an owner seen in a request path, once, as this forge's own account. Repositories created
   * before the login was known are re-owned, so a single-user forge answers under the login the
   * token's requests use without having been told it in advance.
   */
  learnOwner(owner: string): void {
    if (owner === '' || this.viewer === owner) return;
    if (this.viewer !== null) return;
    this.viewer = owner;
    const rekeyed = new Map<string, Repo>();
    for (const repo of this.repos.values()) {
      repo.owner = owner;
      rekeyed.set(this.key(owner, repo.name), repo);
    }
    this.repos.clear();
    for (const [key, repo] of rekeyed) this.repos.set(key, repo);
  }

  login(): string {
    return this.viewer ?? 'viewer';
  }

  user(): User {
    const login = this.login();
    const found = this.users.get(login);
    if (found !== undefined) return found;
    const user: User = {
      id: 1,
      login,
      name: login,
      email: `${login}@example.invalid`,
      type: 'User',
      site_admin: false,
    };
    this.users.set(login, user);
    return user;
  }

  key(owner: string, name: string): string {
    return `${owner.toLowerCase()}/${name.toLowerCase()}`;
  }

  find(owner: string, name: string): Repo | null {
    const repo = this.repos.get(this.key(owner, name)) ?? null;
    if (repo === null) return null;
    // A repository belongs to the login the token names: another owner's is a 404, never a 403.
    if (owner.toLowerCase() !== repo.owner.toLowerCase()) return null;
    return repo;
  }

  createRepo(name: string, description: string, isPrivate: boolean): Repo {
    const owner = this.login();
    const existing = this.repos.get(this.key(owner, name));
    if (existing !== undefined) throw new Conflict('a repository with that name already exists');
    const now = this.clock.tick();
    const repo: Repo = {
      id: this.nextRepoId++,
      name,
      owner,
      description,
      private: isPrivate,
      default_branch: 'main',
      created_at: now,
      updated_at: now,
      pushed_at: now,
      issues: [],
      pulls: [],
      labels: [],
      next_issue: 1,
      next_pull: 1,
      objects: new Map(),
      refs: new Map(),
      packCache: new Map(),
      bodyCache: new Map(),
    };
    this.repos.set(this.key(owner, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.key(owner, name));
  }

  reposOf(owner: string): Repo[] {
    return [...this.repos.values()]
      .filter((r) => r.owner.toLowerCase() === owner.toLowerCase())
      .sort((a, b) => b.id - a.id);
  }

  newIssueId(): number {
    return this.nextIssueId++;
  }
  newCommentId(): number {
    return this.nextCommentId++;
  }
  newPullId(): number {
    return this.nextPullId++;
  }
  newLabelId(): number {
    return this.nextLabelId++;
  }
}

export class Conflict extends Error {}

/** The single store a process serves. */
export const store = new Store();
