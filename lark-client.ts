/**
 * Lark API Client — shared between lark-channel.ts and supervisor.ts
 */

export interface LarkClientConfig {
  readonly appId: string;
  readonly appSecret: string;
  readonly baseUrl: string;
}

export interface LarkClient {
  readonly get: (path: string, params?: Record<string, string>) => Promise<unknown>;
  readonly post: (path: string, body: unknown, params?: Record<string, string>) => Promise<unknown>;
}

export function createLarkClient(config: LarkClientConfig): LarkClient {
  let appAccessToken = "";
  let tokenExpiresAt = 0;
  let refreshPromise: Promise<string> | null = null;

  async function doRefreshToken(): Promise<string> {
    const res = await fetch(
      `${config.baseUrl}/auth/v3/app_access_token/internal`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json; charset=utf-8" },
        body: JSON.stringify({ app_id: config.appId, app_secret: config.appSecret }),
      },
    );
    if (!res.ok) {
      const body = await res.text().catch(() => "");
      throw new Error(`[lark-client] auth HTTP ${res.status}: ${body.slice(0, 500)}`);
    }
    const data = (await res.json()) as {
      code: number;
      msg: string;
      app_access_token: string;
      expire: number;
    };
    if (data.code !== 0) {
      throw new Error(`[lark-client] auth: ${data.msg}`);
    }
    appAccessToken = data.app_access_token;
    tokenExpiresAt = Date.now() + data.expire * 1000;
    return appAccessToken;
  }

  async function getAccessToken(): Promise<string> {
    if (appAccessToken && Date.now() < tokenExpiresAt - 300_000) {
      return appAccessToken;
    }
    if (!refreshPromise) {
      refreshPromise = doRefreshToken().finally(() => { refreshPromise = null; });
    }
    return refreshPromise;
  }

  async function larkFetch(url: string, init: RequestInit): Promise<unknown> {
    const res = await fetch(url, init);
    if (!res.ok) {
      const body = await res.text().catch(() => "");
      throw new Error(`[lark-client] HTTP ${res.status}: ${body.slice(0, 500)}`);
    }
    return res.json();
  }

  async function larkGet(path: string, params?: Record<string, string>) {
    const token = await getAccessToken();
    const url = new URL(`${config.baseUrl}${path}`);
    if (params) {
      for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v);
    }
    return larkFetch(url.toString(), {
      headers: { Authorization: `Bearer ${token}` },
    });
  }

  async function larkPost(path: string, body: unknown, params?: Record<string, string>) {
    const token = await getAccessToken();
    const url = new URL(`${config.baseUrl}${path}`);
    if (params) {
      for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v);
    }
    return larkFetch(url.toString(), {
      method: "POST",
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/json; charset=utf-8",
      },
      body: JSON.stringify(body),
    });
  }

  return { get: larkGet, post: larkPost };
}
