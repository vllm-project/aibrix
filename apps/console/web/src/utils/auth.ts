import { getAuthConfig, getUserInfo, logout as apiLogout } from './api';
import type { UserInfo } from './api';

export interface AuthState {
  loading: boolean;
  user: UserInfo | null;
  authMode: string;
}

export async function checkAuth(): Promise<{ user: UserInfo | null; mode: string }> {
  const config = await getAuthConfig();
  if (config.mode === 'dev') {
    const user = await getUserInfo();
    return { user, mode: config.mode };
  }
  try {
    const user = await getUserInfo();
    return { user, mode: config.mode };
  } catch {
    return { user: null, mode: config.mode };
  }
}

// redirectToOIDCLogin sends the browser to the backend login endpoint, which
// 302-redirects to the configured OIDC provider. The current path is
// preserved as `return` so the user lands back where they started.
export function redirectToOIDCLogin(returnTo?: string): void {
  const target = returnTo ?? window.location.pathname + window.location.search;
  window.location.assign(
    `/api/v1/auth/login?return=${encodeURIComponent(target)}`,
  );
}

export async function handleLogout(): Promise<void> {
  try {
    const resp = await apiLogout();
    if (resp.redirectUrl) {
      // OIDC RP-initiated logout: hand the browser to the IdP so the SSO
      // session is also terminated.
      window.location.assign(resp.redirectUrl);
      return;
    }
  } catch {
    // Local cookie is best-effort cleared by the server even on error;
    // fall through to the reload so the SPA re-bootstraps unauthenticated.
  }
  window.location.reload();
}
