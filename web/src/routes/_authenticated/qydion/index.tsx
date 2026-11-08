import { createFileRoute, redirect } from '@tanstack/react-router'

import QydionConsole from '@/features/qydion'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

// QYDION 运营台路由（M4）：仅管理员可见
export const Route = createFileRoute('/_authenticated/qydion/')({
  beforeLoad: () => {
    const { auth } = useAuthStore.getState()

    if (!auth.user || auth.user.role < ROLE.ADMIN) {
      throw redirect({
        to: '/403',
      })
    }
  },
  component: QydionConsole,
})
