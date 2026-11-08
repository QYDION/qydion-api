/*
 * QYDION 退款管理（交接报告 §E）：管理端退款单列表 / 发起退款（弹窗+二次确认）/
 * 悬挂退款单关闭兜底。
 * 独立落点：features/qydion/refund.tsx，路由 /qydion-refund（仅管理员）。
 * 后端契约（router/qydion-router.go:65-67，controller/qydion_refund.go）：
 *   POST /api/qydion/refund/create {trade_no, refund_money(元,两位小数,>0), reason?}
 *        —— 响应即终态（本网关 alipay_f2f/wx_native 为同步渠道）；失败 message 含原因且额度已回补
 *   GET  /api/qydion/refund/list?p=&page_size=&trade_no=（items 含 username/order_money/
 *        order_status/quota_snapshot，按 id 倒序）
 *   POST /api/qydion/refund/close {refund_id}（仅 pending 可关，关闭即原额回补并置 failed）
 * 状态语义：pending=预扣中（渠道调用在途/进程中断悬挂，可关闭兜底）；
 * succeeded=已受理（凭证号在 channel_credential，微信 PROCESSING 亦记为受理成功）；
 * failed=渠道拒绝/手动关闭（额度已原额回补，不计入累计封顶）。
 * 剩余可退 = order_money −（同单 refund_cents 累计，仅 pending+succeeded，failed 不计）。
 * 文案：中文直接作 i18n key（zh 环境即中文；如需英文翻译在 en.json 补 key 即可）。
 */
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { api } from '@/lib/api'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Textarea } from '@/components/ui/textarea'
import { formatTimestampToDate } from '@/lib/format'

type RefundRow = {
  id: number
  trade_no: string
  user_id: number
  channel: string
  out_request_no: string
  refund_cents: number
  refund_quota: number
  status: string
  channel_credential: string
  operator: string
  note: string
  created_time: number
  updated_time: number
  username?: string
  order_money?: number
  order_status?: string
  quota_snapshot?: number
}

type ListResp = {
  items: RefundRow[]
  total: number
  page: number
  page_size: number
}

type CreateResult = {
  id: number
  trade_no: string
  out_request_no: string
  refund_cents: number
  refund_quota: number
  status: string
  channel: string
  channel_credential: string
}

const errText = (e: unknown): string =>
  (e as { response?: { data?: { message?: string } } })?.response?.data
    ?.message ?? String(e)

const centsToYuan = (cents?: number | null): string =>
  typeof cents === 'number' ? (cents / 100).toFixed(2) : '-'

// 金额（元）客户端校验：>0 且最多两位小数（与服务端 controller/qydion_refund.go:37-43 同纪律）
const isValidRefundMoney = (v: string): boolean =>
  /^\d+(\.\d{1,2})?$/.test(v) && Number(v) > 0

// 渠道展示名：退款渠道枚举见 model/qydion_refund.go:19-20
const channelLabel = (channel: string): string => {
  if (channel === 'alipay') return '支付宝'
  if (channel === 'wechat') return '微信'
  return channel || '-'
}

function StatusBadge({ status }: { status: string }) {
  const { t } = useTranslation()
  if (status === 'succeeded') {
    return <Badge>{t('已受理')}</Badge>
  }
  if (status === 'pending') {
    return (
      <Badge className='border-amber-500/40 bg-amber-500/10 text-amber-600 dark:text-amber-400'>
        {t('预扣中')}
      </Badge>
    )
  }
  return <Badge variant='destructive'>{t('失败/已关闭')}</Badge>
}

// 同单累计（当前页可见记录）：仅 pending+succeeded 计入，failed 不计
// （对齐 model/qydion_refund.go:47-61 的封顶口径）
function activeCentsByTrade(items: RefundRow[]): Record<string, number> {
  const sums: Record<string, number> = {}
  for (const r of items) {
    if (r.status === 'pending' || r.status === 'succeeded') {
      sums[r.trade_no] = (sums[r.trade_no] ?? 0) + r.refund_cents
    }
  }
  return sums
}

// ---------- 关闭悬挂退款单（进程中断兜底） ----------
function ClosePendingDialog(props: {
  row: RefundRow | null
  onClose: () => void
}) {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const [submitting, setSubmitting] = useState(false)
  const open = props.row !== null

  const doClose = async () => {
    if (!props.row) return
    setSubmitting(true)
    try {
      await api.post('/api/qydion/refund/close', { refund_id: props.row.id })
      toast.success(t('已关闭悬挂退款单（额度已原额回补）'))
      qc.invalidateQueries({ queryKey: ['qydion-refund-list'] })
      props.onClose()
    } catch (e) {
      toast.error(errText(e))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <AlertDialog open={open} onOpenChange={(o) => !o && props.onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t('关闭悬挂退款单')}</AlertDialogTitle>
          <AlertDialogDescription>
            {t(
              '退款单 #{{id}}（订单 {{trade_no}}，预扣 ¥{{money}}）。仅用于渠道调用在途或进程中断悬挂单的兜底清理：关闭后按预扣额原额回补额度并置为 failed；渠道侧是否实际退款以渠道对账为准。',
              {
                id: props.row?.id ?? '',
                trade_no: props.row?.trade_no ?? '',
                money: centsToYuan(props.row?.refund_cents),
              },
            )}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={submitting}>
            {t('再想想')}
          </AlertDialogCancel>
          <AlertDialogAction
            disabled={submitting}
            onClick={(e) => {
              e.preventDefault()
              doClose()
            }}
          >
            {t('确认关闭')}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

// ---------- 发起退款（弹窗 + 二次确认） ----------
function RefundCreateDialog(props: { open: boolean; onClose: () => void }) {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const [tradeNo, setTradeNo] = useState('')
  const [money, setMoney] = useState('')
  const [reason, setReason] = useState('')
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [formErr, setFormErr] = useState('')
  const [orderInfo, setOrderInfo] = useState<ListResp | null>(null)
  const [orderMsg, setOrderMsg] = useState('')
  const [result, setResult] = useState<CreateResult | null>(null)

  const reset = () => {
    setTradeNo('')
    setMoney('')
    setReason('')
    setFormErr('')
    setOrderInfo(null)
    setOrderMsg('')
    setResult(null)
    setConfirmOpen(false)
  }

  const close = () => {
    props.onClose()
    reset()
  }

  // 尽力而为的订单预填：复用退款列表接口按订单号过滤（后端契约未提供单独的订单查询端点；
  // 订单尚无任何退款记录时列表为空，此时仍允许提交，由服务端校验订单有效性）
  const queryOrder = async () => {
    setOrderMsg('')
    setOrderInfo(null)
    if (!tradeNo.trim()) {
      setOrderMsg(t('请先填写订单号'))
      return
    }
    try {
      const params = new URLSearchParams({
        p: '1',
        page_size: '100',
        trade_no: tradeNo.trim(),
      })
      const r = await api.get<{ data: ListResp }>(
        `/api/qydion/refund/list?${params.toString()}`,
      )
      const data = r.data.data
      if (!data.items || data.items.length === 0) {
        setOrderMsg(
          t('该订单暂无退款记录，无法预填订单信息；仍可提交，服务端将校验订单有效性。'),
        )
        return
      }
      setOrderInfo(data)
      // 便捷预填：默认退满剩余可退
      const sums = activeCentsByTrade(data.items)
      const first = data.items[0]
      if (typeof first.order_money === 'number' && !money) {
        const remaining = Math.max(
          0,
          Math.round(first.order_money * 100) - (sums[first.trade_no] ?? 0),
        )
        if (remaining > 0) setMoney((remaining / 100).toFixed(2))
      }
    } catch (e) {
      setOrderMsg(errText(e))
    }
  }

  const orderSummary = useMemo(() => {
    if (!orderInfo || orderInfo.items.length === 0) return null
    const sums = activeCentsByTrade(orderInfo.items)
    const first = orderInfo.items[0]
    const orderCents =
      typeof first.order_money === 'number'
        ? Math.round(first.order_money * 100)
        : null
    return {
      orderMoney: first.order_money,
      orderStatus: first.order_status ?? '-',
      quotaSnapshot: first.quota_snapshot,
      refundedActive: sums[first.trade_no] ?? 0,
      remaining:
        orderCents !== null
          ? Math.max(0, orderCents - (sums[first.trade_no] ?? 0))
          : null,
    }
  }, [orderInfo])

  const submit = async () => {
    setFormErr('')
    if (!tradeNo.trim()) {
      setFormErr(t('请填写订单号'))
      return
    }
    if (!isValidRefundMoney(money)) {
      setFormErr(t('退款金额须为大于 0 且最多两位小数的金额（元）'))
      return
    }
    setSubmitting(true)
    try {
      const r = await api.post<{ data: CreateResult }>(
        '/api/qydion/refund/create',
        {
          trade_no: tradeNo.trim(),
          refund_money: Number(money),
          reason: reason.trim() || undefined,
        },
      )
      setResult(r.data.data)
      qc.invalidateQueries({ queryKey: ['qydion-refund-list'] })
      qc.invalidateQueries({ queryKey: ['qydion-refund-order'] })
      toast.success(
        t('退款已受理：{{out_request_no}}', {
          out_request_no: r.data.data.out_request_no,
        }),
      )
    } catch (e) {
      // 失败时服务端已回补额度，message 即为可直接展示的原因
      setFormErr(errText(e))
    } finally {
      setSubmitting(false)
      setConfirmOpen(false)
    }
  }

  return (
    <>
      <Dialog
        open={props.open}
        onOpenChange={(o) => {
          if (!o) close()
        }}
      >
        <DialogContent className='sm:max-w-lg'>
          <DialogHeader>
            <DialogTitle>{t('创建退款')}</DialogTitle>
            <DialogDescription>
              {t(
                '仅本网关支付宝当面付（alipay_f2f）/微信 Native（wx_native）已支付订单支持在线退款；对公订单请走线下人工退款。多次部分退款累计不得超过订单实付。',
              )}
            </DialogDescription>
          </DialogHeader>

          {result ? (
            <div className='space-y-3'>
              <p className='text-sm font-medium text-emerald-600 dark:text-emerald-400'>
                {t('退款已受理（响应即终态）')}
              </p>
              <div className='text-muted-foreground space-y-1 text-xs'>
                <p>
                  {t('退款单号')}：{result.out_request_no}
                </p>
                <p>
                  {t('渠道凭证')}：{result.channel_credential || '-'}
                </p>
                <p>
                  {t('退款金额')}：¥{centsToYuan(result.refund_cents)}
                </p>
                <p>
                  {t('扣减额度')}：{result.refund_quota}
                </p>
              </div>
              <DialogFooter>
                <Button onClick={close}>{t('关闭')}</Button>
              </DialogFooter>
            </div>
          ) : (
            <div className='space-y-3'>
              <div className='space-y-1.5'>
                <Label htmlFor='qydion-refund-trade-no'>{t('订单号')}</Label>
                <div className='flex gap-2'>
                  <Input
                    id='qydion-refund-trade-no'
                    className='font-mono text-xs'
                    placeholder={t('QYD…（本网关订单号）')}
                    value={tradeNo}
                    onChange={(e) => setTradeNo(e.target.value)}
                  />
                  <Button
                    type='button'
                    variant='outline'
                    size='sm'
                    className='shrink-0'
                    onClick={queryOrder}
                  >
                    {t('查询订单信息')}
                  </Button>
                </div>
              </div>

              {orderMsg && (
                <p className='text-muted-foreground text-xs'>{orderMsg}</p>
              )}

              {orderSummary && (
                <div className='bg-muted/30 rounded-md p-3 text-xs leading-relaxed'>
                  <p>
                    {t('订单实付')}：¥{orderSummary.orderMoney?.toFixed(2) ?? '-'}
                    {' · '}
                    {t('订单状态')}：{orderSummary.orderStatus}
                  </p>
                  <p>
                    {t('到账额度快照')}：{orderSummary.quotaSnapshot ?? '-'}
                    {' · '}
                    {t('已退累计（不含失败/关闭）')}：¥
                    {centsToYuan(orderSummary.refundedActive)}
                  </p>
                  <p className='font-medium'>
                    {t('剩余可退')}：¥
                    {orderSummary.remaining !== null
                      ? (orderSummary.remaining / 100).toFixed(2)
                      : '-'}
                  </p>
                </div>
              )}

              <div className='space-y-1.5'>
                <Label htmlFor='qydion-refund-money'>
                  {t('退款金额（元，两位小数，>0）')}
                </Label>
                <Input
                  id='qydion-refund-money'
                  inputMode='decimal'
                  placeholder='10.00'
                  value={money}
                  onChange={(e) => setMoney(e.target.value.trim())}
                />
              </div>

              <div className='space-y-1.5'>
                <Label htmlFor='qydion-refund-reason'>
                  {t('退款原因（可选）')}
                </Label>
                <Textarea
                  id='qydion-refund-reason'
                  rows={2}
                  value={reason}
                  onChange={(e) => setReason(e.target.value)}
                />
              </div>

              {formErr && <p className='text-xs text-red-500'>{formErr}</p>}

              <DialogFooter>
                <Button variant='outline' onClick={close}>
                  {t('取消')}
                </Button>
                <Button
                  disabled={submitting}
                  onClick={() => {
                    if (!tradeNo.trim()) {
                      setFormErr(t('请填写订单号'))
                      return
                    }
                    if (!isValidRefundMoney(money)) {
                      setFormErr(t('退款金额须为大于 0 且最多两位小数的金额（元）'))
                      return
                    }
                    setFormErr('')
                    setConfirmOpen(true)
                  }}
                >
                  {t('提交退款')}
                </Button>
              </DialogFooter>
            </div>
          )}
        </DialogContent>
      </Dialog>

      {/* 二次确认：渠道同步受理，响应即终态 */}
      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t('确认发起退款')}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(
                '订单 {{trade_no}} 将退款 ¥{{money}}。本网关为同步渠道，渠道受理即终态：成功后按订单快照单价扣减对应额度并记入额度流水；失败时额度自动原额回补。',
                { trade_no: tradeNo.trim(), money: money || '0.00' },
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={submitting}>
              {t('再想想')}
            </AlertDialogCancel>
            <AlertDialogAction
              disabled={submitting}
              className='bg-destructive text-white hover:bg-destructive/90 dark:bg-destructive/60'
              onClick={(e) => {
                e.preventDefault()
                submit()
              }}
            >
              {t('确认退款')}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

// ---------- 页面 ----------
export default function QydionRefundAdmin() {
  const { t } = useTranslation()
  const [tradeNoInput, setTradeNoInput] = useState('')
  const [appliedTradeNo, setAppliedTradeNo] = useState('')
  const [usernameFilter, setUsernameFilter] = useState('')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [createOpen, setCreateOpen] = useState(false)
  const [closingRow, setClosingRow] = useState<RefundRow | null>(null)

  const listQuery = useQuery({
    queryKey: ['qydion-refund-list', appliedTradeNo, page, pageSize],
    queryFn: async () => {
      const params = new URLSearchParams({
        p: String(page),
        page_size: String(pageSize),
      })
      if (appliedTradeNo) params.set('trade_no', appliedTradeNo)
      const r = await api.get<{ data: ListResp }>(
        `/api/qydion/refund/list?${params.toString()}`,
      )
      return r.data.data
    },
  })

  const items = useMemo(() => {
    const rows = listQuery.data?.items ?? []
    const kw = usernameFilter.trim().toLowerCase()
    if (!kw) return rows
    // 用户筛选为当前页内过滤（后端契约仅提供 trade_no 服务端过滤）
    return rows.filter(
      (r) =>
        (r.username ?? '').toLowerCase().includes(kw) ||
        String(r.user_id) === kw,
    )
  }, [listQuery.data, usernameFilter])

  const sums = useMemo(() => activeCentsByTrade(items), [items])

  const total = listQuery.data?.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / pageSize))

  return (
    <div className='mx-auto w-full max-w-6xl px-4 py-6'>
      <div className='mb-4 flex flex-wrap items-center justify-between gap-3'>
        <h1 className='text-xl font-semibold'>{t('退款管理')}</h1>
        <Button size='sm' onClick={() => setCreateOpen(true)}>
          {t('发起退款')}
        </Button>
      </div>

      <div className='mb-3 flex flex-wrap items-end gap-2'>
        <div className='space-y-1'>
          <Label htmlFor='qydion-refund-filter-trade'>{t('订单号（精确匹配）')}</Label>
          <Input
            id='qydion-refund-filter-trade'
            className='h-8 w-64 font-mono text-xs'
            placeholder={t('QYD…（留空查全部）')}
            value={tradeNoInput}
            onChange={(e) => setTradeNoInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                setPage(1)
                setAppliedTradeNo(tradeNoInput.trim())
              }
            }}
          />
        </div>
        <div className='space-y-1'>
          <Label htmlFor='qydion-refund-filter-user'>
            {t('用户名（当前页内过滤）')}
          </Label>
          <Input
            id='qydion-refund-filter-user'
            className='h-8 w-48 text-xs'
            value={usernameFilter}
            onChange={(e) => setUsernameFilter(e.target.value)}
          />
        </div>
        <div className='space-y-1'>
          <Label htmlFor='qydion-refund-page-size'>{t('每页条数')}</Label>
          <NativeSelect
            id='qydion-refund-page-size'
            className='h-8 w-24 text-xs'
            value={String(pageSize)}
            onChange={(e) => {
              setPageSize(Number(e.target.value))
              setPage(1)
            }}
          >
            {[20, 50, 100].map((n) => (
              <NativeSelectOption key={n} value={String(n)}>
                {n}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
        <Button
          size='sm'
          className='h-8'
          onClick={() => {
            setPage(1)
            setAppliedTradeNo(tradeNoInput.trim())
          }}
        >
          {t('查询')}
        </Button>
        {(appliedTradeNo || usernameFilter) && (
          <Button
            size='sm'
            variant='ghost'
            className='h-8'
            onClick={() => {
              setTradeNoInput('')
              setUsernameFilter('')
              setAppliedTradeNo('')
              setPage(1)
            }}
          >
            {t('清除筛选')}
          </Button>
        )}
      </div>

      <p className='text-muted-foreground mb-3 text-xs'>
        {t(
          '状态语义：pending=预扣中（渠道调用在途/进程中断悬挂，可关闭兜底）；succeeded=已受理（微信受理成功即记账，渠道状态见备注）；failed=渠道拒绝/手动关闭，额度已原额回补、不计入累计封顶。',
        )}
      </p>

      <Card>
        <CardContent className='pt-4'>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>#</TableHead>
                <TableHead>{t('订单号 / 订单信息')}</TableHead>
                <TableHead>{t('用户')}</TableHead>
                <TableHead>{t('渠道')}</TableHead>
                <TableHead>{t('退款金额（元）/ 扣减额度')}</TableHead>
                <TableHead>{t('状态')}</TableHead>
                <TableHead>{t('凭证 / 备注')}</TableHead>
                <TableHead>{t('操作人')}</TableHead>
                <TableHead>{t('时间')}</TableHead>
                <TableHead>{t('操作')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {items.map((r) => {
                const remainingCents =
                  typeof r.order_money === 'number'
                    ? Math.max(
                        0,
                        Math.round(r.order_money * 100) -
                          (sums[r.trade_no] ?? 0),
                      )
                    : null
                return (
                  <TableRow key={r.id}>
                    <TableCell className='font-mono text-xs'>{r.id}</TableCell>
                    <TableCell>
                      <p className='font-mono text-xs'>{r.trade_no}</p>
                      <p className='text-muted-foreground mt-0.5 text-xs leading-relaxed'>
                        {t('订单实付')}：¥
                        {r.order_money !== undefined
                          ? r.order_money.toFixed(2)
                          : '-'}
                        {' · '}
                        {t('状态')}：{r.order_status ?? '-'}
                        <br />
                        {t('到账额度快照')}：{r.quota_snapshot ?? '-'}
                        {' · '}
                        {t('剩余可退')}：¥
                        {remainingCents !== null
                          ? (remainingCents / 100).toFixed(2)
                          : '-'}
                      </p>
                    </TableCell>
                    <TableCell className='text-xs'>
                      {r.username || `#${r.user_id}`}
                    </TableCell>
                    <TableCell className='text-xs'>
                      {channelLabel(r.channel)}
                    </TableCell>
                    <TableCell>
                      <p className='text-xs font-medium'>
                        ¥{centsToYuan(r.refund_cents)}
                      </p>
                      <p className='text-muted-foreground text-xs'>
                        {t('额度')} {r.refund_quota}
                      </p>
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={r.status} />
                    </TableCell>
                    <TableCell className='max-w-40'>
                      <p className='truncate font-mono text-xs' title={r.channel_credential}>
                        {r.channel_credential || '-'}
                      </p>
                      <p className='text-muted-foreground truncate text-xs' title={r.note}>
                        {r.note || '-'}
                      </p>
                    </TableCell>
                    <TableCell className='text-xs'>{r.operator || '-'}</TableCell>
                    <TableCell className='text-xs'>
                      {formatTimestampToDate(r.created_time)}
                    </TableCell>
                    <TableCell>
                      {r.status === 'pending' && (
                        <Button
                          size='sm'
                          variant='outline'
                          className='h-7 text-xs'
                          onClick={() => setClosingRow(r)}
                        >
                          {t('关闭')}
                        </Button>
                      )}
                    </TableCell>
                  </TableRow>
                )
              })}
              {items.length === 0 && (
                <TableRow>
                  <TableCell
                    colSpan={10}
                    className='text-muted-foreground text-center text-sm'
                  >
                    {listQuery.isLoading ? t('加载中…') : t('暂无退款记录')}
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <div className='mt-3 flex flex-wrap items-center justify-between gap-2'>
        <p className='text-muted-foreground text-xs'>
          {t('第 {{page}} / {{totalPages}} 页 · 共 {{total}} 条', {
            page,
            totalPages,
            total,
          })}
        </p>
        <div className='flex gap-2'>
          <Button
            size='sm'
            variant='outline'
            disabled={page <= 1}
            onClick={() => setPage((p) => Math.max(1, p - 1))}
          >
            {t('上一页')}
          </Button>
          <Button
            size='sm'
            variant='outline'
            disabled={page >= totalPages}
            onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
          >
            {t('下一页')}
          </Button>
        </div>
      </div>

      <p className='text-muted-foreground mt-2 text-xs'>
        {t(
          '剩余可退 = 订单实付 − 累计退款（pending+succeeded 计入，failed 不计）。列表未按订单号筛选时该值基于当前页可见记录累计；按订单号筛选后即为该单全量精确值，服务端在创建退款时仍会强制校验封顶。',
        )}
      </p>

      <RefundCreateDialog
        open={createOpen}
        onClose={() => setCreateOpen(false)}
      />
      <ClosePendingDialog row={closingRow} onClose={() => setClosingRow(null)} />
    </div>
  )
}
