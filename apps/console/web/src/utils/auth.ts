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

export async function handleLogout(): Promise<void> {
  await apiLogout();
  window.location.reload();
}
