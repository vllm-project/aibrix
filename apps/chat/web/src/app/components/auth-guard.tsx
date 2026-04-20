import { Navigate, Outlet } from 'react-router'
import { useAuth } from '@/app/context/auth-context'

export function AuthGuard() {
  const { user, authMode, loading } = useAuth()

  if (loading || authMode === null) {
    return (
      <div className="flex h-screen w-screen items-center justify-center bg-background">
        <div className="text-muted-foreground text-sm">Loading...</div>
      </div>
    )
  }

  // No auth required
  if (authMode === 'none') {
    return <Outlet />
  }

  // Auth required but not logged in
  if (!user) {
    return <Navigate to="/login" replace />
  }

  return <Outlet />
}
