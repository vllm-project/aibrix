import { createContext, type ReactNode, useCallback, useContext, useEffect, useState } from 'react'

export interface AuthUser {
  id: string
  name: string
  created_at: string
}

interface AuthContextValue {
  user: AuthUser | null
  token: string | null
  authMode: 'none' | 'simple' | null // null = loading
  login: (name: string) => Promise<void>
  logout: () => void
  loading: boolean
}

const AuthContext = createContext<AuthContextValue | null>(null)

const TOKEN_KEY = 'aibrix-chat-token'

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<AuthUser | null>(null)
  const [token, setToken] = useState<string | null>(null)
  const [authMode, setAuthMode] = useState<'none' | 'simple' | null>(null)
  const [loading, setLoading] = useState(true)

  // Fetch auth mode and validate existing token on mount
  useEffect(() => {
    let cancelled = false

    async function init() {
      try {
        // 1. Check auth mode
        const modeRes = await fetch('/api/auth/mode')
        if (!modeRes.ok) {
          // If endpoint doesn't exist, assume "none"
          if (!cancelled) {
            setAuthMode('none')
            setLoading(false)
          }
          return
        }
        const modeData = await modeRes.json()
        const mode = modeData.auth_mode as 'none' | 'simple'
        if (!cancelled) setAuthMode(mode)

        if (mode === 'none') {
          // No auth needed — set a default user
          if (!cancelled) {
            setUser({ id: 'default', name: 'User', created_at: '' })
            setLoading(false)
          }
          return
        }

        // 2. If simple auth, try to restore session from localStorage
        const savedToken = localStorage.getItem(TOKEN_KEY)
        if (!savedToken) {
          if (!cancelled) setLoading(false)
          return
        }

        const meRes = await fetch('/api/auth/me', {
          headers: { Authorization: `Bearer ${savedToken}` },
        })
        if (meRes.ok) {
          const userData = await meRes.json()
          if (!cancelled) {
            setUser(userData)
            setToken(savedToken)
          }
        } else {
          // Token invalid — clear it
          localStorage.removeItem(TOKEN_KEY)
        }
      } catch {
        // Network error — just continue unauthenticated
      } finally {
        if (!cancelled) setLoading(false)
      }
    }

    init()
    return () => {
      cancelled = true
    }
  }, [])

  const login = useCallback(async (name: string) => {
    const res = await fetch('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name }),
    })
    if (!res.ok) throw new Error('Login failed')
    const data = await res.json()
    setUser(data.user)
    setToken(data.token)
    localStorage.setItem(TOKEN_KEY, data.token)
  }, [])

  const logout = useCallback(() => {
    const savedToken = token || localStorage.getItem(TOKEN_KEY)
    if (savedToken) {
      fetch('/api/auth/logout', {
        method: 'POST',
        headers: { Authorization: `Bearer ${savedToken}` },
      }).catch(() => {})
    }
    setUser(null)
    setToken(null)
    localStorage.removeItem(TOKEN_KEY)
  }, [token])

  return (
    <AuthContext.Provider value={{ user, token, authMode, login, logout, loading }}>{children}</AuthContext.Provider>
  )
}

export function useAuth() {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
