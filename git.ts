// The git side: the object model, a packfile writer for clone/fetch, a packfile reader for push, and
// the smart-HTTP protocol. No git binary, no dependency: node:zlib and node:crypto only.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';
import type { GitObject, Repo } from './store.ts';

const TYPE_CODE: Record<string, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, GitObject['type']> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export function sha1(data: Buffer | string): string {
  return createHash('sha1').update(data).digest('hex');
}

/** The object id git gives `<type> <size>\0<data>`. */
export function hashObject(type: GitObject['type'], data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return sha1(Buffer.concat([header, data]));
}

export function putObject(repo: Repo, type: GitObject['type'], data: Buffer): string {
  const id = hashObject(type, data);
  if (!repo.objects.has(id)) repo.objects.set(id, { type, data });
  return id;
}

export function readObject(repo: Repo, id: string): GitObject | null {
  return repo.objects.get(id) ?? null;
}

// ---------------------------------------------------------------- trees

export interface TreeEntry {
  mode: string;
  name: string;
  sha: string;
}

/** git's tree sort: a directory sorts as its name plus '/'. */
function treeSortKey(entry: TreeEntry): string {
  return entry.mode === '40000' ? `${entry.name}/` : entry.name;
}

export function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => (treeSortKey(a) < treeSortKey(b) ? -1 : treeSortKey(a) > treeSortKey(b) ? 1 : 0));
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.sha, 'hex'));
  }
  return Buffer.concat(parts);
}

export function parseTree(data: Buffer): TreeEntry[] {
  const out: TreeEntry[] = [];
  let at = 0;
  while (at < data.length) {
    const space = data.indexOf(0x20, at);
    if (space < 0) break;
    const mode = data.subarray(at, space).toString('utf8');
    const nul = data.indexOf(0x00, space);
    if (nul < 0) break;
    const name = data.subarray(space + 1, nul).toString('utf8');
    const sha = data.subarray(nul + 1, nul + 21).toString('hex');
    out.push({ mode, name, sha });
    at = nul + 21;
  }
  return out;
}

/** Every path under a tree, as `path -> {mode, sha}`; recursive. */
export function flattenTree(repo: Repo, treeId: string, prefix = ''): Map<string, { mode: string; sha: string }> {
  const out = new Map<string, { mode: string; sha: string }>();
  const tree = readObject(repo, treeId);
  if (tree === null || tree.type !== 'tree') return out;
  for (const entry of parseTree(tree.data)) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') {
      for (const [k, v] of flattenTree(repo, entry.sha, path)) out.set(k, v);
    } else if (entry.mode === '160000') {
      out.set(path, { mode: entry.mode, sha: entry.sha });
    } else {
      out.set(path, { mode: entry.mode, sha: entry.sha });
    }
  }
  return out;
}

/** Build the nested trees for a flat path map, returning the root tree id. */
export function writeTreeFromPaths(repo: Repo, files: Map<string, { mode: string; sha: string }>): string {
  interface Node {
    files: Map<string, { mode: string; sha: string }>;
    dirs: Map<string, Node>;
  }
  const root: Node = { files: new Map(), dirs: new Map() };
  for (const [path, value] of files) {
    const parts = path.split('/');
    let node = root;
    for (let i = 0; i < parts.length - 1; i += 1) {
      const name = parts[i]!;
      let child = node.dirs.get(name);
      if (child === undefined) {
        child = { files: new Map(), dirs: new Map() };
        node.dirs.set(name, child);
      }
      node = child;
    }
    node.files.set(parts[parts.length - 1]!, value);
  }
  const write = (node: Node): string => {
    const entries: TreeEntry[] = [];
    for (const [name, value] of node.files) entries.push({ mode: value.mode, name, sha: value.sha });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, sha: write(child) });
    return putObject(repo, 'tree', buildTree(entries));
  };
  return write(root);
}

// ---------------------------------------------------------------- commits

export interface CommitMeta {
  tree: string;
  parents: string[];
  message: string;
  author: { name: string; email: string };
  time: string;
}

export function buildCommit(meta: CommitMeta): Buffer {
  const seconds = Math.floor(Date.parse(meta.time) / 1000);
  const zone = '+0000';
  const ident = `${meta.author.name} <${meta.author.email}> ${seconds} ${zone}`;
  const lines = [`tree ${meta.tree}`];
  for (const parent of meta.parents) lines.push(`parent ${parent}`);
  lines.push(`author ${ident}`, `committer ${ident}`, '', meta.message.endsWith('\n') ? meta.message : `${meta.message}\n`);
  return Buffer.from(lines.join('\n'), 'utf8');
}

export interface CommitInfo {
  tree: string;
  parents: string[];
  message: string;
  authorName: string;
  authorEmail: string;
  time: string;
}

export function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const head = text.split('\n\n')[0] ?? '';
  const body = text.slice(head.length + 2);
  let tree = '';
  const parents: string[] = [];
  let authorName = '';
  let authorEmail = '';
  let time = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) {
      const m = /^author (.*) <(.*)> (\d+) ([+-]\d{4})$/.exec(line);
      if (m) {
        authorName = m[1]!;
        authorEmail = m[2]!;
        time = new Date(Number(m[3]) * 1000).toISOString().replace('.000Z', 'Z');
      }
    }
  }
  return { tree, parents, message: body, authorName, authorEmail, time };
}

/** Every commit reachable from a set of tips, tips first. */
export function reachableCommits(repo: Repo, tips: string[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  const stack = [...tips];
  while (stack.length > 0) {
    const id = stack.pop()!;
    if (seen.has(id)) continue;
    const object = readObject(repo, id);
    if (object === null) continue;
    if (object.type === 'tag') {
      const target = tagTarget(object.data);
      if (target !== null) stack.push(target);
      continue;
    }
    if (object.type !== 'commit') continue;
    seen.add(id);
    out.push(id);
    for (const parent of parseCommit(object.data).parents) stack.push(parent);
  }
  return out;
}

export function tagTarget(data: Buffer): string | null {
  for (const line of data.toString('utf8').split('\n')) {
    if (line.startsWith('object ')) return line.slice(7).trim();
    if (line === '') break;
  }
  return null;
}

/** Every object reachable from commits, tags included. */
export function reachableObjects(repo: Repo, tips: string[]): Set<string> {
  const out = new Set<string>();
  const stack = [...tips];
  while (stack.length > 0) {
    const id = stack.pop()!;
    if (out.has(id)) continue;
    const object = readObject(repo, id);
    if (object === null) continue;
    out.add(id);
    if (object.type === 'commit') {
      const info = parseCommit(object.data);
      stack.push(info.tree);
      for (const parent of info.parents) stack.push(parent);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) stack.push(entry.sha);
    } else if (object.type === 'tag') {
      const target = tagTarget(object.data);
      if (target !== null) stack.push(target);
    }
  }
  return out;
}

/** The first common commit of two tips, by walking both ancestries. */
export function mergeBase(repo: Repo, a: string, b: string): string | null {
  const ancestorsOf = (tip: string): Map<string, number> => {
    const depth = new Map<string, number>();
    const queue: Array<[string, number]> = [[tip, 0]];
    while (queue.length > 0) {
      const [id, d] = queue.shift()!;
      if (depth.has(id)) continue;
      const object = readObject(repo, id);
      if (object === null || object.type !== 'commit') continue;
      depth.set(id, d);
      for (const parent of parseCommit(object.data).parents) queue.push([parent, d + 1]);
    }
    return depth;
  };
  const left = ancestorsOf(a);
  const right = ancestorsOf(b);
  let best: string | null = null;
  let bestScore = Number.POSITIVE_INFINITY;
  for (const [id, d] of left) {
    if (!right.has(id)) continue;
    const score = d + (right.get(id) ?? 0);
    if (score < bestScore) {
      bestScore = score;
      best = id;
    }
  }
  return best;
}

// ---------------------------------------------------------------- pack writing

function packObjectHeader(type: GitObject['type'], size: number): Buffer {
  const bytes: number[] = [];
  let value = size;
  let first = (TYPE_CODE[type]! << 4) | (value & 0x0f);
  value >>>= 4;
  while (value > 0) {
    bytes.push(first | 0x80);
    first = value & 0x7f;
    value >>>= 7;
  }
  bytes.push(first);
  return Buffer.from(bytes);
}

/** A packfile (version 2) holding these objects, uncompressed entries, sha1 trailer. */
export function writePack(repo: Repo, ids: Iterable<string>): Buffer {
  const parts: Buffer[] = [];
  const list = [...ids];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(list.length, 8);
  parts.push(header);
  for (const id of list) {
    const object = readObject(repo, id);
    if (object === null) continue;
    parts.push(packObjectHeader(object.type, object.data.length));
    parts.push(deflateSync(object.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, Buffer.from(sha1(body), 'hex')]);
}

// ---------------------------------------------------------------- pack reading

export class PackError extends Error {}

/** Apply a git delta to a base buffer. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let at = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[at++]!;
      value |= (byte & 0x7f) << shift;
      shift += 7;
      if ((byte & 0x80) === 0) break;
    }
    return value;
  };
  const baseSize = readVarint();
  if (baseSize !== base.length) throw new PackError(`delta names a base of ${baseSize} bytes, the base has ${base.length}`);
  const resultSize = readVarint();
  const out = Buffer.alloc(resultSize);
  let written = 0;
  while (at < delta.length) {
    const op = delta[at++]!;
    if ((op & 0x80) !== 0) {
      let offset = 0;
      let size = 0;
      if (op & 0x01) offset |= delta[at++]!;
      if (op & 0x02) offset |= delta[at++]! << 8;
      if (op & 0x04) offset |= delta[at++]! << 16;
      if (op & 0x08) offset |= delta[at++]! << 24;
      if (op & 0x10) size |= delta[at++]!;
      if (op & 0x20) size |= delta[at++]! << 8;
      if (op & 0x40) size |= delta[at++]! << 16;
      if (size === 0) size = 0x10000;
      if (offset + size > base.length) throw new PackError('delta copies past the end of its base');
      base.copy(out, written, offset, offset + size);
      written += size;
    } else if (op !== 0) {
      const size = op;
      delta.copy(out, written, at, at + size);
      at += size;
      written += size;
    } else {
      throw new PackError('delta opcode 0 is reserved');
    }
  }
  if (written !== resultSize) throw new PackError(`delta wrote ${written} bytes, expected ${resultSize}`);
  return out;
}

export interface PackReadResult {
  objects: Map<string, GitObject>;
}

/** Read a packfile into objects, resolving OFS_DELTA and REF_DELTA against the pack and `lookup`. */
export function readPack(buffer: Buffer, lookup: (id: string) => GitObject | null): PackReadResult {
  if (buffer.length < 32) throw new PackError('pack is shorter than its header and trailer');
  if (buffer.subarray(0, 4).toString('ascii') !== 'PACK') throw new PackError('pack has no PACK signature');
  const version = buffer.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new PackError(`pack version ${version} is not 2 or 3`);
  const count = buffer.readUInt32BE(8);
  const body = buffer.subarray(0, buffer.length - 20);
  const trailer = buffer.subarray(buffer.length - 20).toString('hex');
  if (sha1(body) !== trailer) throw new PackError('pack trailer checksum does not match');

  const objects = new Map<string, GitObject>();
  const byOffset = new Map<number, { type: GitObject['type']; data: Buffer }>();
  let at = 12;
  for (let index = 0; index < count; index += 1) {
    const entryOffset = at;
    let byte = buffer[at++]!;
    const typeCode = (byte >> 4) & 0x07;
    let size = byte & 0x0f;
    let shift = 4;
    while ((byte & 0x80) !== 0) {
      byte = buffer[at++]!;
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    let baseOffset = -1;
    let baseId: string | null = null;
    if (typeCode === 6) {
      // OFS_DELTA: a negative offset varint
      let byte2 = buffer[at++]!;
      let value = byte2 & 0x7f;
      while ((byte2 & 0x80) !== 0) {
        byte2 = buffer[at++]!;
        value = ((value + 1) << 7) | (byte2 & 0x7f);
      }
      baseOffset = entryOffset - value;
    } else if (typeCode === 7) {
      baseId = buffer.subarray(at, at + 20).toString('hex');
      at += 20;
    }
    const start = at;
    const inflated = inflateAt(buffer, start);
    at = inflated.end;
    const data = inflated.data;
    if (typeCode === 6) {
      const base = byOffset.get(baseOffset);
      if (base === undefined) throw new PackError(`ofs-delta at ${entryOffset} names a base not in the pack`);
      const resolved = applyDelta(base.data, data);
      byOffset.set(entryOffset, { type: base.type, data: resolved });
      objects.set(hashObject(base.type, resolved), { type: base.type, data: resolved });
    } else if (typeCode === 7) {
      const found = (baseId !== null ? objects.get(baseId) : undefined) ?? (baseId !== null ? lookup(baseId) : null);
      if (found === null || found === undefined) throw new PackError(`ref-delta names a base ${baseId} that is not available`);
      const resolved = applyDelta(found.data, data);
      objects.set(hashObject(found.type, resolved), { type: found.type, data: resolved });
      byOffset.set(entryOffset, { type: found.type, data: resolved });
    } else {
      const type = CODE_TYPE[typeCode];
      if (type === undefined) throw new PackError(`pack entry ${index} has unknown type ${typeCode}`);
      if (data.length !== size) throw new PackError(`pack entry ${index} inflates to ${data.length} bytes, header says ${size}`);
      byOffset.set(entryOffset, { type, data });
      objects.set(hashObject(type, data), { type, data });
    }
  }
  return { objects };
}

/** Inflate one zlib stream at `start`, returning the bytes and the offset just past it. */
function inflateAt(buffer: Buffer, start: number): { data: Buffer; end: number } {
  const chunk = buffer.subarray(start);
  const result = inflateSync(chunk, { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
  const consumed = result.engine.bytesWritten;
  if (!(consumed > 0) || consumed > chunk.length) throw new PackError('a pack entry has no complete zlib stream');
  return { data: result.buffer, end: start + consumed };
}

// ---------------------------------------------------------------- pkt-lines

export function pktLine(text: string | Buffer): Buffer {
  const payload = typeof text === 'string' ? Buffer.from(text, 'utf8') : text;
  const length = payload.length + 4;
  return Buffer.concat([Buffer.from(length.toString(16).padStart(4, '0'), 'ascii'), payload]);
}

export const FLUSH = Buffer.from('0000', 'ascii');
export const DELIM = Buffer.from('0001', 'ascii');

/** Decode pkt-lines into payloads; `null` marks a flush, `undefined` a delim. */
export function readPktLines(buffer: Buffer): Array<Buffer | null | undefined> {
  const out: Array<Buffer | null | undefined> = [];
  let at = 0;
  while (at + 4 <= buffer.length) {
    const length = Number.parseInt(buffer.subarray(at, at + 4).toString('ascii'), 16);
    if (Number.isNaN(length)) break;
    if (length === 0) {
      out.push(null);
      at += 4;
      continue;
    }
    if (length === 1) {
      out.push(undefined);
      at += 4;
      continue;
    }
    if (length === 2) {
      out.push(undefined);
      at += 4;
      continue;
    }
    if (at + length > buffer.length) break;
    out.push(buffer.subarray(at + 4, at + length));
    at += length;
  }
  return out;
}

/** The offset just past the pkt-lines at the head of `buffer`. */
export function pktLinesEnd(buffer: Buffer): number {
  let at = 0;
  while (at + 4 <= buffer.length) {
    const length = Number.parseInt(buffer.subarray(at, at + 4).toString('ascii'), 16);
    if (Number.isNaN(length)) break;
    at += length === 0 || length === 1 || length === 2 ? 4 : length;
  }
  return at;
}

// ---------------------------------------------------------------- shallow

/** Commits reachable from `tips` within `depth`, and the boundary those at the edge. */
export function shallowWalk(repo: Repo, tips: string[], depth: number): { include: Set<string>; boundary: Set<string> } {
  const include = new Set<string>();
  const boundary = new Set<string>();
  const seen = new Map<string, number>();
  const queue: Array<[string, number]> = tips.map((tip) => [tip, 1]);
  while (queue.length > 0) {
    const [id, level] = queue.shift()!;
    const previous = seen.get(id);
    if (previous !== undefined && previous <= level) continue;
    seen.set(id, level);
    const object = readObject(repo, id);
    if (object === null || object.type !== 'commit') continue;
    include.add(id);
    const parents = parseCommit(object.data).parents;
    if (level >= depth) {
      boundary.add(id);
      continue;
    }
    for (const parent of parents) queue.push([parent, level + 1]);
  }
  return { include, boundary };
}
