// git smart HTTP: upload-pack protocol v2 (ls-refs, fetch, shallow) and receive-pack v0 (push with
// report-status over side-band-64k). Everything is served from the forge's own object store.

import type { IncomingMessage, ServerResponse } from 'node:http';
import type { Repo } from './store.ts';
import {
  FLUSH,
  DELIM,
  PackError,
  buildCommit,
  mergeBase,
  parseCommit,
  parseTree,
  pktLine,
  pktLinesEnd,
  putObject,
  readObject,
  readPack,
  readPktLines,
  reachableCommits,
  reachableObjects,
  shallowWalk,
  writePack,
  writeTreeFromPaths,
} from './git.ts';

const AGENT = 'agent=makefun-forge/1.0';
const ZERO = '0000000000000000000000000000000000000000';

export interface GitRequest {
  repo: Repo;
  method: string;
  path: string;
  query: URLSearchParams;
  headers: IncomingMessage['headers'];
  body: Buffer;
  res: ServerResponse;
}

function writeBuffer(res: ServerResponse, status: number, type: string, body: Buffer, extra: Record<string, string> = {}): void {
  res.writeHead(status, {
    'content-type': type,
    'content-length': body.length,
    'cache-control': 'no-cache',
    ...extra,
  });
  res.end(body);
}

/** Refuse a request with the JSON shape every other route uses. */
function notFound(res: ServerResponse): void {
  const body = Buffer.from(`${JSON.stringify({ message: 'Not Found' })}\n`);
  writeBuffer(res, 404, 'application/json; charset=utf-8', body);
}

/** Every ref this repository has, sorted: heads then tags, each by name. */
function refList(repo: Repo): Array<{ name: string; sha: string }> {
  const refs: Array<{ name: string; sha: string }> = [];
  for (const [name, sha] of repo.refs) refs.push({ name, sha });
  refs.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return refs;
}

/** The commit a ref peels to: through any depth of annotated tags, or itself when it is a commit. */
function peel(repo: Repo, sha: string): string | null {
  let current = sha;
  for (let guard = 0; guard < 16; guard += 1) {
    const object = readObject(repo, current);
    if (object === null) return null;
    if (object.type === 'commit') return current;
    if (object.type !== 'tag') return null;
    let next: string | null = null;
    for (const line of object.data.toString('utf8').split('\n')) {
      if (line.startsWith('object ')) {
        next = line.slice(7).trim();
        break;
      }
      if (line === '') break;
    }
    if (next === null) return null;
    current = next;
  }
  return null;
}

export function handleInfoRefs(req: GitRequest): void {
  const service = req.query.get('service') ?? '';
  if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
    notFound(req.res);
    return;
  }
  const wantsV2 = service === 'git-upload-pack' && String(req.headers['git-protocol'] ?? '').includes('version=2');
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), FLUSH];

  if (wantsV2) {
    parts.push(pktLine('version 2\n'));
    parts.push(pktLine(`${AGENT}\n`));
    parts.push(pktLine('ls-refs=unborn\n'));
    parts.push(pktLine('fetch=shallow wait-for-done\n'));
    parts.push(pktLine('server-option\n'));
    parts.push(pktLine('object-format=sha1\n'));
    parts.push(FLUSH);
    writeBuffer(req.res, 200, `application/x-${service}-advertisement`, Buffer.concat(parts));
    return;
  }

  const refs = refList(req.repo);
  const caps =
    service === 'git-receive-pack'
      ? `${AGENT} report-status side-band-64k delete-refs ofs-delta`
      : `${AGENT} side-band-64k ofs-delta shallow no-progress`;
  if (refs.length === 0) {
    parts.push(pktLine(Buffer.concat([Buffer.from(`${ZERO} capabilities^{}\0${caps}\n`, 'utf8')])));
  } else {
    let first = true;
    for (const ref of refs) {
      const line = first ? `${ref.sha} ${ref.name}\0${caps}\n` : `${ref.sha} ${ref.name}\n`;
      first = false;
      parts.push(pktLine(line));
      const peeled = peel(req.repo, ref.sha);
      if (peeled !== null && peeled !== ref.sha) parts.push(pktLine(`${peeled} ${ref.name}^{}\n`));
    }
  }
  parts.push(FLUSH);
  writeBuffer(req.res, 200, `application/x-${service}-advertisement`, Buffer.concat(parts));
}

// ---------------------------------------------------------------- upload-pack

interface FetchRequest {
  wants: string[];
  haves: string[];
  done: boolean;
  depth: number | null;
  shallow: string[];
  refPrefixes: string[];
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
}

export function handleUploadPack(req: GitRequest): void {
  const packets = readPktLines(req.body);
  const commands: Buffer[] = [];
  const args: string[] = [];
  let phase: 'command' | 'args' = 'command';
  const seen = new Set<string>();
  for (const packet of packets) {
    if (packet === null || packet === undefined) {
      phase = 'args';
      continue;
    }
    const text = packet.toString('utf8');
    if (phase === 'command') {
      const trimmed = text.replace(/\n$/, '');
      if (trimmed.startsWith('command=')) {
        const name = trimmed.slice(8);
        if (seen.has(name)) continue;
        seen.add(name);
        commands.push(packet);
      } else if (trimmed !== '') {
        commands.push(packet);
      }
    } else {
      args.push(text.replace(/\n$/, ''));
    }
  }

  const isLsRefs = commands.some((c) => c.toString('utf8').includes('command=ls-refs'));
  if (isLsRefs) {
    handleLsRefs(req, args);
    return;
  }
  const isFetch = commands.some((c) => c.toString('utf8').includes('command=fetch'));
  if (isFetch) {
    handleFetch(req, args);
    return;
  }
  writeBuffer(req.res, 200, 'application/x-git-upload-pack-result', FLUSH);
}

function handleLsRefs(req: GitRequest, args: string[]): void {
  const lsKey = `ls|${[...args].sort().join('|')}`;
  const lsCached = req.repo.bodyCache.get(lsKey);
  if (lsCached !== undefined) {
    writeBuffer(req.res, 200, 'application/x-git-upload-pack-result', lsCached);
    return;
  }
  let peelWanted = false;
  let symrefs = false;
  let unborn = false;
  const prefixes: string[] = [];
  for (const arg of args) {
    if (arg === 'peel') peelWanted = true;
    else if (arg === 'symrefs') symrefs = true;
    else if (arg === 'unborn') unborn = true;
    else if (arg.startsWith('ref-prefix ')) prefixes.push(arg.slice(11).trim());
  }
  const matches = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
  const out: Buffer[] = [];
  const refs = refList(req.repo).filter((ref) => matches(ref.name));
  const head = req.repo.refs.get(`refs/heads/${req.repo.default_branch}`);

  if (refs.length === 0 && unborn && matches('HEAD')) {
    out.push(pktLine(`unborn HEAD symref-target:refs/heads/${req.repo.default_branch}\n`));
  } else if (head !== undefined && matches('HEAD')) {
    out.push(pktLine(`${head} HEAD${symrefs ? ` symref-target:refs/heads/${req.repo.default_branch}` : ''}\n`));
  }
  for (const ref of refs) {
    // v2 carries the peeled oid as an attribute on the ref's own line; a separate `^{}` line is the
    // v0 advertisement's convention, not this one.
    const peeled = peelWanted ? peel(req.repo, ref.sha) : null;
    const suffix = peeled !== null && peeled !== ref.sha ? ` peeled:${peeled}` : '';
    out.push(pktLine(`${ref.sha} ${ref.name}${suffix}\n`));
  }
  out.push(FLUSH);
  const lsBody = Buffer.concat(out);
  if (req.repo.bodyCache.size > 32) req.repo.bodyCache.clear();
  req.repo.bodyCache.set(lsKey, lsBody);
  writeBuffer(req.res, 200, 'application/x-git-upload-pack-result', lsBody);
}

function parseFetch(args: string[]): FetchRequest {
  const request: FetchRequest = { wants: [], haves: [], done: false, depth: null, shallow: [], refPrefixes: [], peel: false, symrefs: false, unborn: false };
  for (const arg of args) {
    if (arg.startsWith('want ')) request.wants.push(arg.slice(5).trim());
    else if (arg.startsWith('have ')) request.haves.push(arg.slice(5).trim());
    else if (arg === 'done') request.done = true;
    else if (arg.startsWith('deepen ')) request.depth = Number.parseInt(arg.slice(7).trim(), 10);
    else if (arg.startsWith('shallow ')) request.shallow.push(arg.slice(8).trim());
  }
  return request;
}

function handleFetch(req: GitRequest, args: string[]): void {
  const request = parseFetch(args);
  const repo = req.repo;
  const out: Buffer[] = [];

  // acknowledgments: the client's haves, when it sent any
  if (request.haves.length > 0) {
    out.push(pktLine('acknowledgments\n'));
    const known = request.haves.filter((id) => readObject(repo, id) !== null);
    for (const id of known) out.push(pktLine(`ACK ${id}\n`));
    if (known.length === 0) out.push(pktLine('NAK\n'));
    else out.push(pktLine('ready\n'));
    out.push(FLUSH);
  }

  // shallow-info, when the client asked to deepen or is already shallow
  const wantCommits = request.wants.filter((id) => readObject(repo, id) !== null);
  let include: Set<string> | null = null;
  let boundary: Set<string> = new Set();
  if (request.depth !== null) {
    const walk = shallowWalk(repo, wantCommits, Math.max(1, request.depth));
    include = walk.include;
    boundary = walk.boundary;
    out.push(pktLine('shallow-info\n'));
    for (const id of boundary) out.push(pktLine(`shallow ${id}\n`));
    for (const id of request.shallow) if (!boundary.has(id)) out.push(pktLine(`unshallow ${id}\n`));
    out.push(FLUSH);
  } else if (request.shallow.length > 0) {
    out.push(pktLine('shallow-info\n'));
    for (const id of request.shallow) out.push(pktLine(`unshallow ${id}\n`));
    out.push(FLUSH);
  }

  // The pack: everything the wants reach, minus everything the haves reach. Fifty shallow clones of
  // one branch ask the same question, so the answer is built once and reused until a ref moves.
  const cacheKey = `${[...request.wants].sort().join(',')}|${request.depth ?? ''}|${[...request.haves].sort().join(',')}|${[...request.shallow].sort().join(',')}`;
  const cachedBody = repo.bodyCache.get(`fetch|${cacheKey}`);
  if (cachedBody !== undefined) {
    writeBuffer(req.res, 200, 'application/x-git-upload-pack-result', cachedBody);
    return;
  }
  const cached = repo.packCache.get(cacheKey);
  if (cached !== undefined) {
    out.push(pktLine('packfile\n'));
    for (let at = 0; at < cached.length; at += 65_000) {
      out.push(pktLine(Buffer.concat([Buffer.from([1]), cached.subarray(at, Math.min(at + 65_000, cached.length))])));
    }
    out.push(FLUSH);
    const body = Buffer.concat(out);
    if (repo.bodyCache.size > 32) repo.bodyCache.clear();
    repo.bodyCache.set(`fetch|${cacheKey}`, body);
    writeBuffer(req.res, 200, 'application/x-git-upload-pack-result', body);
    return;
  }

  const haveObjects = new Set<string>();
  const shallowClient = request.shallow.length > 0;
  for (const id of request.haves) {
    if (readObject(repo, id) === null) continue;
    if (shallowClient) {
      // The client's boundary commit is all it has: its own tree and blobs, not its ancestors.
      haveObjects.add(id);
      const object = readObject(repo, id);
      if (object !== null && object.type === 'commit') {
        for (const treeObject of reachableTree(repo, parseCommit(object.data).tree)) haveObjects.add(treeObject);
      }
    } else {
      for (const object of reachableObjects(repo, [id])) haveObjects.add(object);
    }
  }
  const tips = include === null ? wantCommits : [...include];
  const wanted = reachableObjects(repo, tips);
  const send: string[] = [];
  for (const id of wanted) {
    if (haveObjects.has(id)) continue;
    if (include !== null) {
      const object = readObject(repo, id);
      if (object !== null && object.type === 'commit' && !include.has(id)) continue;
    }
    send.push(id);
  }
  const pack = writePack(repo, send);
  if (repo.packCache.size > 24) repo.packCache.clear();
  repo.bodyCache.clear();
  repo.packCache.set(cacheKey, pack);

  out.push(pktLine('packfile\n'));
  for (let at = 0; at < pack.length; at += 65_000) {
    const chunk = pack.subarray(at, Math.min(at + 65_000, pack.length));
    out.push(pktLine(Buffer.concat([Buffer.from([1]), chunk])));
  }
  out.push(FLUSH);
  const fetchBody = Buffer.concat(out);
  if (repo.bodyCache.size > 32) repo.bodyCache.clear();
  repo.bodyCache.set(`fetch|${cacheKey}`, fetchBody);
  writeBuffer(req.res, 200, 'application/x-git-upload-pack-result', fetchBody);
}

// ---------------------------------------------------------------- receive-pack

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

export function handleReceivePack(req: GitRequest): void {
  const repo = req.repo;
  const packets = readPktLines(req.body);
  const commands: PushCommand[] = [];
  let caps = new Set<string>();
  let first = true;
  for (const packet of packets) {
    if (packet === null || packet === undefined) break;
    let text = packet.toString('utf8').replace(/\n$/, '');
    if (first) {
      const nul = text.indexOf('\u0000');
      if (nul >= 0) {
        caps = new Set(text.slice(nul + 1).split(' ').filter((c) => c !== ''));
        text = text.slice(0, nul);
      }
      first = false;
    }
    const parts = text.split(' ');
    if (parts.length < 3) continue;
    commands.push({ oldOid: parts[0]!, newOid: parts[1]!, ref: parts.slice(2).join(' ') });
  }

  const packStart = pktLinesEnd(req.body);
  let unpackOk = true;
  let unpackDetail = '';
  const packBytes = req.body.subarray(packStart);
  if (packBytes.length > 0) {
    try {
      const read = readPack(packBytes, (id) => readObject(repo, id));
      for (const [id, object] of read.objects) repo.objects.set(id, object);
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof PackError ? err.message : 'the pack could not be read';
    }
  }

  const report: string[] = [`unpack ${unpackOk ? 'ok' : `error ${unpackDetail}`}\n`];
  for (const command of commands) {
    const current = repo.refs.get(command.ref) ?? null;
    const isDelete = command.newOid === ZERO;
    const isCreate = command.oldOid === ZERO;
    if (!unpackOk) {
      report.push(`ng ${command.ref} unpacker error\n`);
      continue;
    }
    if (isCreate) {
      if (current !== null) {
        report.push(`ng ${command.ref} already exists\n`);
        continue;
      }
      if (isDelete) {
        report.push(`ng ${command.ref} cannot delete a ref that does not exist\n`);
        continue;
      }
      if (readObject(repo, command.newOid) === null) {
        report.push(`ng ${command.ref} missing object\n`);
        continue;
      }
      repo.refs.set(command.ref, command.newOid);
      repo.packCache.clear();
  repo.bodyCache.clear();
      report.push(`ok ${command.ref}\n`);
      continue;
    }
    if (current !== command.oldOid) {
      report.push(`ng ${command.ref} fetch first\n`);
      continue;
    }
    if (isDelete) {
      repo.refs.delete(command.ref);
      repo.packCache.clear();
  repo.bodyCache.clear();
      report.push(`ok ${command.ref}\n`);
      continue;
    }
    if (readObject(repo, command.newOid) === null) {
      report.push(`ng ${command.ref} missing object\n`);
      continue;
    }
    if (!isAncestor(repo, command.oldOid, command.newOid)) {
      report.push(`ng ${command.ref} non-fast-forward\n`);
      continue;
    }
    repo.refs.set(command.ref, command.newOid);
    repo.packCache.clear();
  repo.bodyCache.clear();
    report.push(`ok ${command.ref}\n`);
  }

  const body = Buffer.concat(report.map((line) => pktLine(line)).concat([FLUSH]));
  if (caps.has('side-band-64k')) {
    const framed: Buffer[] = [];
    for (let at = 0; at < body.length; at += 65_000) {
      const chunk = body.subarray(at, Math.min(at + 65_000, body.length));
      framed.push(pktLine(Buffer.concat([Buffer.from([1]), chunk])));
    }
    framed.push(FLUSH);
    writeBuffer(req.res, 200, 'application/x-git-receive-pack-result', Buffer.concat(framed));
    return;
  }
  writeBuffer(req.res, 200, 'application/x-git-receive-pack-result', body);
}

/** A tree, its subtrees and their blobs; never a commit. */
function reachableTree(repo: Repo, treeId: string): Set<string> {
  const out = new Set<string>();
  const stack = [treeId];
  while (stack.length > 0) {
    const id = stack.pop()!;
    if (out.has(id)) continue;
    const object = readObject(repo, id);
    if (object === null) continue;
    out.add(id);
    if (object.type === 'tree') for (const entry of parseTree(object.data)) stack.push(entry.sha);
  }
  return out;
}

/** Whether `ancestor` is reachable from `descendant` by walking parents. */
export function isAncestor(repo: Repo, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>();
  const stack = [descendant];
  while (stack.length > 0) {
    const id = stack.pop()!;
    if (seen.has(id)) continue;
    seen.add(id);
    if (id === ancestor) return true;
    const object = readObject(repo, id);
    if (object === null || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) stack.push(parent);
  }
  return false;
}

/** Write a file through the git object graph, returning the new commit id. */
export function commitFile(
  repo: Repo,
  branch: string,
  files: Map<string, { mode: string; sha: string }>,
  message: string,
  author: { name: string; email: string },
  time: string,
  parents: string[],
): string {
  const tree = writeTreeFromPaths(repo, files);
  const commit = putObject(repo, 'commit', buildCommit({ tree, parents, message, author, time }));
  repo.refs.set(`refs/heads/${branch}`, commit);
  return commit;
}
