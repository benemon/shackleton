import type { components } from './generated/api';

export type Summary = components['schemas']['Summary'];
export type StoredEvent = components['schemas']['Event'];
export type Investigation = components['schemas']['Investigation'];
export type PendingApproval = components['schemas']['PendingApproval'];
export type ApprovalEvent = components['schemas']['ApprovalEvent'];
export type ConfigView = components['schemas']['ConfigView'];
export type Inventory = components['schemas']['Inventory'];
export type Verdict = components['schemas']['Verdict'];
export type KBArticleMeta = components['schemas']['KBArticleMeta'];

const TOKEN_KEY = 'shackleton-token';

export const getToken = () => sessionStorage.getItem(TOKEN_KEY);
export const setToken = (t: string) => sessionStorage.setItem(TOKEN_KEY, t);
export const clearToken = () => sessionStorage.removeItem(TOKEN_KEY);

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { ...(init?.headers ?? {}), Authorization: `Bearer ${getToken() ?? ''}` },
  });
  if (res.status === 401) {
    clearToken();
    window.location.reload();
    throw new Error('unauthorized');
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(body.error ?? res.statusText);
  }
  return res.json() as Promise<T>;
}

export const api = {
  listInvestigations: () => request<Summary[]>('/v1/investigations'),
  getInvestigation: (id: string) => request<Investigation>(`/v1/investigations/${encodeURIComponent(id)}`),
  createInvestigation: (question: string) =>
    request<Summary>('/v1/investigations', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ question }),
    }),
  listApprovals: () => request<PendingApproval[]>('/v1/approvals'),
  decideApproval: (id: string, approved: boolean) =>
    request<{ approved: boolean }>(`/v1/approvals/${encodeURIComponent(id)}/decision`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ approved }),
    }),
  getConfig: () => request<ConfigView>('/v1/config'),
  getInventory: () => request<Inventory>('/v1/inventory'),
  listKB: () => request<KBArticleMeta[]>('/v1/kb'),
  // Raw markdown, not JSON.
  getKBArticle: async (slug: string): Promise<string> => {
    const res = await fetch(`/v1/kb/${encodeURIComponent(slug)}`, {
      headers: { Authorization: `Bearer ${getToken() ?? ''}` },
    });
    if (!res.ok) throw new Error(res.statusText);
    return res.text();
  },
};

// EventSource cannot send an Authorization header, so SSE runs over a fetch
// stream parsed frame by frame.
export async function streamSSE(
  path: string,
  onEvent: (type: string, data: string) => void,
  signal: AbortSignal,
): Promise<void> {
  const res = await fetch(path, {
    headers: { Authorization: `Bearer ${getToken() ?? ''}` },
    signal,
  });
  if (!res.ok || !res.body) throw new Error(`stream failed: ${res.status}`);
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  for (;;) {
    const { done, value } = await reader.read();
    if (done) return;
    buffer += decoder.decode(value, { stream: true });
    let sep;
    while ((sep = buffer.indexOf('\n\n')) >= 0) {
      const frame = buffer.slice(0, sep);
      buffer = buffer.slice(sep + 2);
      let type = 'message';
      let data = '';
      for (const line of frame.split('\n')) {
        if (line.startsWith('event: ')) type = line.slice(7);
        else if (line.startsWith('data: ')) data += line.slice(6);
      }
      if (data !== '') onEvent(type, data);
    }
  }
}
