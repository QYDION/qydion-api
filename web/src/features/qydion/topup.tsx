/*
 * QYDION 在线充值（M7 §8.6b）：支付宝当面付 / 微信 Native（扫码）+ Stripe（跳转）
 * 独立落点：features/qydion/topup.tsx，路由 /qydion-topup（全部登录用户）。
 */
import { useQuery } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { api } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'

type Method = { name: string; display_name: string; enabled: boolean; mode: string }
type OrderResp = { trade_no: string; money: number; status: string; pay_url: string; qr_content: string; expires_in: number }

export default function QydionTopup() {
  const { t } = useTranslation()
  const [provider, setProvider] = useState('')
  const [money, setMoney] = useState('100')
  const [coupon, setCoupon] = useState('')
  const [order, setOrder] = useState<OrderResp | null>(null)
  const [payStatus, setPayStatus] = useState('')
  const [msg, setMsg] = useState('')
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)

  const methods = useQuery({
    queryKey: ['qydion-pay-methods'],
    queryFn: async () => (await api.get<{ data: Method[] }>('/api/qydion/payment/methods')).data.data,
  })
  const enabled = (methods.data ?? []).filter((m) => m.enabled)
  useEffect(() => {
    if (!provider && enabled.length > 0) setProvider(enabled[0].name)
  }, [enabled, provider])

  useEffect(() => () => { if (pollRef.current) clearInterval(pollRef.current) }, [])

  const stopPoll = () => { if (pollRef.current) { clearInterval(pollRef.current); pollRef.current = null } }

  const startPoll = (tradeNo: string) => {
    stopPoll()
    pollRef.current = setInterval(async () => {
      try {
        const r = (await api.get<{ data: { trade_no: string; status: string } }>(
          `/api/qydion/payment/status?trade_no=${encodeURIComponent(tradeNo)}`)).data.data
        setPayStatus(r.status)
        if (r.status !== 'pending') stopPoll()
      } catch { /* 网络抖动继续轮询 */ }
    }, 3000)
  }

  const submit = async () => {
    setMsg('')
    stopPoll()
    setPayStatus('')
    try {
      const r = await api.post<{ data: OrderResp }>('/api/qydion/payment/create', {
        provider, money: Number(money), coupon_code: coupon.trim() || undefined,
      })
      const o = r.data.data
      setOrder(o)
      setPayStatus(o.status)
      if (o.status === 'pending') startPoll(o.trade_no)
    } catch (e) {
      const m = (e as { response?: { data?: { message?: string } } })?.response?.data?.message ?? String(e)
      setMsg(m)
    }
  }

  const cur = enabled.find((m) => m.name === provider)

  return (
    <div className='mx-auto w-full max-w-3xl px-4 py-6'>
      <h1 className='mb-4 text-xl font-semibold'>{t('在线充值')}</h1>

      <Card>
        <CardHeader><CardTitle className='text-sm'>{t('选择支付方式')}</CardTitle></CardHeader>
        <CardContent className='flex flex-wrap gap-2'>
          {enabled.length === 0 && (
            <p className='text-muted-foreground text-xs'>{t('暂无可用支付渠道（管理员尚未配置支付参数）')}</p>
          )}
          {enabled.map((m) => (
            <Button
              key={m.name}
              size='sm'
              variant={provider === m.name ? 'default' : 'outline'}
              onClick={() => { setProvider(m.name); setOrder(null); stopPoll(); setPayStatus('') }}
            >
              {t(m.display_name)}
            </Button>
          ))}
        </CardContent>
      </Card>

      {cur && (
        <Card className='mt-4'>
          <CardHeader><CardTitle className='text-sm'>{t('充值金额')}</CardTitle></CardHeader>
          <CardContent className='space-y-3'>
            <div className='flex flex-wrap gap-2'>
              {[50, 100, 200, 500, 1000].map((v) => (
                <Button key={v} size='sm' variant={money === String(v) ? 'default' : 'outline'} onClick={() => setMoney(String(v))}>
                  ¥{v}
                </Button>
              ))}
            </div>
            <div className='flex flex-wrap items-center gap-2'>
              <Input className='h-8 w-40' placeholder={t('自定义金额（元）')} value={money} onChange={(e) => setMoney(e.target.value)} />
              <Input className='h-8 w-44' placeholder={t('优惠券码（可选）')} value={coupon} onChange={(e) => setCoupon(e.target.value)} />
              <Button size='sm' onClick={submit}>{t('下一步')}</Button>
            </div>
            <p className='text-muted-foreground text-xs'>{t('订单 30 分钟内有效，超时自动关闭；到账后可用额度即时生效。')}</p>
            {msg && <p className='text-xs text-red-500'>{msg}</p>}
          </CardContent>
        </Card>
      )}

      {order && (
        <Card className='mt-4'>
          <CardHeader>
            <CardTitle className='text-sm'>
              {t('订单')} {order.trade_no}
              {payStatus && <span className='ml-2 text-xs'>
                {payStatus === 'pending' ? t('待支付…') : payStatus === 'success' ? t('✅ 已到账') : t(payStatus)}
              </span>}
            </CardTitle>
          </CardHeader>
          <CardContent className='space-y-2'>
            <p className='text-sm'>{t('应付金额')}: ¥{order.money.toFixed(2)}</p>
            {payStatus === 'pending' && cur.mode === 'qr' && order.qr_content && (
              <div className='space-y-2'>
                <p className='text-muted-foreground text-xs'>{t('请使用对应 App 扫码支付（复制下方链接到手机打开亦可）：')}</p>
                <div className='rounded-md border p-2 font-mono text-xs break-all'>{order.qr_content}</div>
                <Button size='sm' variant='outline' onClick={() => navigator.clipboard?.writeText(order.qr_content)}>
                  {t('复制支付链接')}
                </Button>
              </div>
            )}
            {payStatus === 'pending' && cur.mode === 'redirect' && order.pay_url && (
              <div className='space-y-2'>
                <p className='text-muted-foreground text-xs'>{t('即将跳转到支付页面：')}</p>
                <Button size='sm' onClick={() => window.open(order.pay_url, '_blank', 'noopener')}>{t('前往支付')}</Button>
              </div>
            )}
            {payStatus === 'success' && <p className='text-xs text-emerald-500'>{t('充值已到账，额度已入账（含满送/券折已按规则处理）。')}</p>}
            {(payStatus === 'cancelled' || payStatus === 'expired') && <p className='text-xs text-red-500'>{t('订单已关闭（超时未支付），请重新下单。')}</p>}
          </CardContent>
        </Card>
      )}
    </div>
  )
}
