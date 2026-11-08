import { createFileRoute, redirect } from '@tanstack/react-router'

import QydionRefundAdmin from '@/features/qydion/refund'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

// QYDION 退款管理路由（交接报告 §E）：仅管理员可见
export const Route = createFileRoute('/_authenticated/qydion-refund/')({
  beforeLoad: () => {
    const { auth } = useAuthStore.getState()

    if (!auth.user || auth.user.role < ROLE.ADMIN) {
      throw redirect({
        to: '/403',
      })
    }
  },
  component: QydionRefundAdmin,
})
