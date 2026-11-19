// The forge's HTTP surface: one process, four protocols on one port, one store behind them.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { store } from './store.ts';
import { handleRest, sendJson } from './rest.ts';
import { handleInfoRefs, handleReceivePack, handleUploadPack, type GitRequest } from './githttp.ts';
import { handleGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';

const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

/** Read the whole body; a request that never ends is bounded by the trial's wall anyway. */
async function readBody(req: IncomingMessage): Promise<Buffer> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk as Buffer);
  return Buffer.concat(chunks);
}

function pathAndQuery(req: IncomingMessage): { path: string; query: URLSearchParams } {
  try {
    const url = new URL(req.url ?? '/', 'http://forge');
    return { path: url.pathname, query: url.searchParams };
  } catch {
    return { path: '/', query: new URLSearchParams() };
  }
}

/** The bearer token a request presents, or null. */
function bearer(req: IncomingMessage): string | null {
  const header = req.headers.authorization;
  if (typeof header !== 'string') return null;
  const match = /^Bearer\s+(.+)$/i.exec(header.trim());
  return match === null ? null : match[1]!.trim();
}

function authorised(req: IncomingMessage): boolean {
  const presented = bearer(req);
  if (presented === null || presented === '') return false;
  if (TOKEN === '') return true;
  return presented === TOKEN;
}

function jsonBody(body: Buffer): { ok: true; value: unknown } | { ok: false } {
  if (body.length === 0) return { ok: true, value: {} };
  try {
    return { ok: true, value: JSON.parse(body.toString('utf8')) };
  } catch {
    return { ok: false };
  }
}

export function createApp(): Server {
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void handle(req, res).catch((err: unknown) => {
      if (!res.headersSent) {
        sendJson(res, 500, { message: err instanceof Error ? err.message : 'internal error' });
      } else {
        res.end();
      }
    });
  });
}

async function handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const { path, query } = pathAndQuery(req);
  const method = (req.method ?? 'GET').toUpperCase();

  // The env's boot check calls this twice, with no token.
  if (method === 'GET' && path === '/healthz') {
    sendJson(res, 200, { ok: true });
    return;
  }

  const body = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);

  // git smart HTTP: /{owner}/{repo}.git/...
  const gitMatch = /^\/([^/]+)\/([^/]+)\.git(\/.*)?$/.exec(path);
  if (gitMatch !== null) {
    const owner = decodeURIComponent(gitMatch[1]!);
    const name = decodeURIComponent(gitMatch[2]!);
    const rest = gitMatch[3] ?? '';
    store.learnOwner(owner);
    const repo = store.find(owner, name);
    const gitReq: GitRequest = { repo: repo as never, method, path, query, headers: req.headers, body, res };
    if (repo === null) {
      sendJson(res, 404, { message: 'Not Found' });
      return;
    }
    if (method === 'GET' && rest === '/info/refs') {
      handleInfoRefs(gitReq);
      return;
    }
    if (method === 'POST' && rest === '/git-upload-pack') {
      handleUploadPack(gitReq);
      return;
    }
    if (method === 'POST' && rest === '/git-receive-pack') {
      handleReceivePack(gitReq);
      return;
    }
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }

  if (path === '/mcp' || path.startsWith('/mcp/')) {
    if (!authorised(req)) {
      sendJson(res, 401, { message: 'Requires authentication' });
      return;
    }
    await handleMcp({ req, res, method, body, json: () => jsonBody(body), token: bearer(req) });
    return;
  }

  if (path === '/api/graphql' || path === '/api/graphql/') {
    if (!authorised(req)) {
      sendJson(res, 401, { message: 'Requires authentication' });
      return;
    }
    await handleGraphql({ req, res, method, body, token: bearer(req) });
    return;
  }

  if (path === '/api/v3' || path.startsWith('/api/v3/')) {
    if (!authorised(req)) {
      sendJson(res, 401, { message: 'Requires authentication' });
      return;
    }
    const sub = path === '/api/v3' ? '' : path.slice('/api/v3'.length);
    const parsed = jsonBody(body);
    let handled = false;
    try {
      handled = handleRest({
        req,
        res,
        method,
        path: sub,
        query,
        body,
        json: () => {
          if (!parsed.ok) throw new BadJson();
          return parsed.value;
        },
        token: bearer(req),
      });
    } catch (err) {
      if (err instanceof BadJson) {
        sendJson(res, 400, { message: 'Problems parsing JSON' });
        return;
      }
      throw err;
    }
    if (!handled) {
      sendJson(res, 404, { message: 'Not Found' });
    }
    return;
  }

  sendJson(res, 404, { message: 'Not Found' });
}

/** A body that is not JSON, thrown by the json() accessor and answered as 400 by the caller. */
export class BadJson extends Error {}

export { TOKEN };
