import { Outlet } from 'react-router'
import { AuthProvider } from '@/app/context/auth-context'

/**
 * Root layout that provides AuthContext to the entire app.
 * All routes are children of this component.
 */
export function AuthLayout() {
  return (
    <AuthProvider>
      <Outlet />
    </AuthProvider>
  )
}
