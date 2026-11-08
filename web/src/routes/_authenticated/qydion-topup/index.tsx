import { createFileRoute } from '@tanstack/react-router'

import QydionTopup from '@/features/qydion/topup'

// QYDION 在线充值路由（M7 §8.6b）：全部登录用户可用（_authenticated 已做登录门控）
export const Route = createFileRoute('/_authenticated/qydion-topup/')({
  component: QydionTopup,
})
