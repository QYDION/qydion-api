/*
 * QYDION 运营台（M4，规格书 §8.2b/§8.4/§8.5c/§8.6c/§8.7/§8.8）
 * 独立落点：features/qydion/，路由挂 /qydion。
 * 文案：中文直接作 i18n key（zh 环境即中文；如需英文翻译在 en.json 补 key 即可）。
 */
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { api } from '@/lib/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from '@/components/ui/table'
import { Textarea } from '@/components/ui/textarea'

type Status = { ok?: boolean; msg?: string }

function useQydionApi() {
  const [status, setStatus] = useState<Status>({})
  const call = async <T,>(fn: () => Promise<T>, okMsg: string): Promise<T | undefined> => {
    try {
      const r = await fn()
      setStatus({ ok: true, msg: okMsg })
      return r
    } catch (e) {
      const msg = (e as { response?: { data?: { message?: string } } })?.response?.data?.message ?? String(e)
      setStatus({ ok: false, msg })
      return undefined
    }
  }
  return { status, setStatus, call }
}

function downloadBlob(url: string, filename: string) {
  api
    .get(url, { responseType: 'blob' })
    .then((res) => {
      const blob = new Blob([res.data as BlobPart])
      const a = document.createElement('a')
      a.href = URL.createObjectURL(blob)
      a.download = filename
      a.click()
      URL.revokeObjectURL(a.href)
    })
    .catch(() => {})
}

// ---------- 统计 ----------
function StatsPanel() {
  const { t } = useTranslation()
  const cards = useQuery({
    queryKey: ['qydion-stats-cards'],
    queryFn: async () => (await api.get<{ data: Record<string, number> }>('/api/qydion/stats/cards')).data.data,
    refetchInterval: 30000,
  })
  const trend = useQuery({
    queryKey: ['qydion-stats-trend'],
    queryFn: async () =>
      (await api.get<{ data: Array<{ day: string; quota: number; tokens: number; requests: number }> }>('/api/qydion/stats/trend?days=7')).data.data,
  })
  const byModel = useQuery({
    queryKey: ['qydion-stats-model'],
    queryFn: async () =>
      (await api.get<{ data: Array<{ dimension: string; requests: number; in_tokens: number; out_tokens: number; income_cny: number }> }>('/api/qydion/stats/by-model?days=7')).data.data,
  })
  const c = cards.data ?? {}
  return (
    <div className='space-y-4'>
      <div className='grid grid-cols-2 gap-3 md:grid-cols-5'>
        {[
          ['今日收入(元)', c.today_income_cny],
          ['今日消费 tokens', c.today_tokens],
          ['今日新增用户', c.today_new_users],
          ['今日活跃用户', c.today_active_users],
          ['待审核对公单', c.pending_corporate_cnt],
        ].map(([label, value]) => (
          <Card key={label as string}>
            <CardHeader className='pb-1'>
              <CardTitle className='text-muted-foreground text-xs font-normal'>{t(label as string)}</CardTitle>
            </CardHeader>
            <CardContent className='text-xl font-semibold'>{String(value ?? '-')}</CardContent>
          </Card>
        ))}
      </div>
      <Card>
        <CardHeader><CardTitle className='text-sm'>近 7 天趋势（收入/请求）</CardTitle></CardHeader>
        <CardContent>
          <Table>
            <TableHeader><TableRow><TableHead>日期</TableHead><TableHead>收入(元)</TableHead><TableHead>tokens</TableHead><TableHead>请求数</TableHead></TableRow></TableHeader>
            <TableBody>
              {(trend.data ?? []).map((r) => (
                <TableRow key={r.day}><TableCell>{r.day}</TableCell><TableCell>{r.quota}</TableCell><TableCell>{r.tokens}</TableCell><TableCell>{r.requests}</TableCell></TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
      <Card>
        <CardHeader><CardTitle className='text-sm'>近 7 天按模型（CSV 导出见下方按钮组）</CardTitle></CardHeader>
        <CardContent>
          <div className='mb-2 flex gap-2'>
            <Button size='sm' variant='outline' onClick={() => downloadBlob('/api/qydion/stats/export?dim=model&days=7', 'qydion-model.csv')}>导出按模型 CSV</Button>
            <Button size='sm' variant='outline' onClick={() => downloadBlob('/api/qydion/stats/export?dim=user&days=7', 'qydion-user.csv')}>按用户</Button>
            <Button size='sm' variant='outline' onClick={() => downloadBlob('/api/qydion/stats/export?dim=channel&days=7', 'qydion-channel.csv')}>按渠道</Button>
          </div>
          <Table>
            <TableHeader><TableRow><TableHead>模型</TableHead><TableHead>请求数</TableHead><TableHead>输入</TableHead><TableHead>输出</TableHead><TableHead>收入(元)</TableHead></TableRow></TableHeader>
            <TableBody>
              {(byModel.data ?? []).slice(0, 20).map((r) => (
                <TableRow key={r.dimension}><TableCell>{r.dimension}</TableCell><TableCell>{r.requests}</TableCell><TableCell>{r.in_tokens}</TableCell><TableCell>{r.out_tokens}</TableCell><TableCell>{r.income_cny}</TableCell></TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}

// ---------- 模型维护扩展 ----------
function ModelExtPanel() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const { status, setStatus, call } = useQydionApi()
  const list = useQuery({
    queryKey: ['qydion-model-ext'],
    queryFn: async () =>
      (await api.get<{ data: Array<Record<string, unknown>> }>('/api/qydion/model-ext')).data.data,
  })
  const draft = useRef<Record<string, { display_name: string; sort_order: string; pricing_note: string }>>({})
  const save = async (name: string) => {
    const d = draft.current[name] ?? {
      display_name: String(list.data?.find((r) => r.model_name === name)?.display_name ?? ''),
      sort_order: String(list.data?.find((r) => r.model_name === name)?.sort_order ?? 0),
      pricing_note: String(list.data?.find((r) => r.model_name === name)?.pricing_note ?? ''),
    }
    await call(
      () => api.post('/api/qydion/model-ext', {
        model_name: name,
        display_name: d.display_name,
        sort_order: Number(d.sort_order) || 0,
        pricing_note: d.pricing_note,
      }),
      '已保存',
    )
    qc.invalidateQueries({ queryKey: ['qydion-model-ext'] })
  }
  return (
    <div className='space-y-3'>
      {status.msg && <p className={status.ok ? 'text-xs text-emerald-500' : 'text-xs text-red-500'}>{status.msg}</p>}
      <p className='text-muted-foreground text-xs'>{t('渠道已配置模型与元数据的并集；展示名/排序/计费说明存 QYDION 扩展表（不动原生表）。')}</p>
      <Card>
        <CardContent className='max-h-[520px] overflow-auto pt-4'>
          <Table>
            <TableHeader><TableRow><TableHead>模型名</TableHead><TableHead>渠道数</TableHead><TableHead>展示名</TableHead><TableHead>排序</TableHead><TableHead>计费说明</TableHead><TableHead></TableHead></TableRow></TableHeader>
            <TableBody>
              {(list.data ?? []).slice(0, 100).map((r) => {
                const name = String(r.model_name)
                const d = (draft.current[name] ??= {
                  display_name: String(r.display_name ?? ''),
                  sort_order: String(r.sort_order ?? 0),
                  pricing_note: String(r.pricing_note ?? ''),
                })
                return (
                  <TableRow key={name}>
                    <TableCell className='font-mono text-xs'>{name}</TableCell>
                    <TableCell>{String(r.channel_count)}</TableCell>
                    <TableCell><Input className='h-8 w-32' value={d.display_name} onChange={(e) => { d.display_name = e.target.value }} /></TableCell>
                    <TableCell><Input className='h-8 w-16' value={d.sort_order} onChange={(e) => { d.sort_order = e.target.value }} /></TableCell>
                    <TableCell><Input className='h-8 w-40' value={d.pricing_note} onChange={(e) => { d.pricing_note = e.target.value }} /></TableCell>
                    <TableCell><Button size='sm' onClick={() => save(name)}>{t('保存修改')}</Button></TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}

// ---------- 价格表 ----------
function PricingPanel() {
  const { t } = useTranslation()
  const { status, setStatus, call } = useQydionApi()
  const fileRef = useRef<HTMLInputElement>(null)
  const doImport = async () => {
    const file = fileRef.current?.files?.[0]
    if (!file) {
      setStatus({ ok: false, msg: '请选择 CSV 文件（列：模型名,输入价(元/百万),输出价(元/百万),计费方式）' })
      return
    }
    // 全量安全审计 P3：前端预校验（与后端 2MB/5000 行上限对齐，减少误传）
    if (!file.name.toLowerCase().endsWith('.csv') || file.size > 2 * 1024 * 1024) {
      setStatus({ ok: false, msg: '仅支持 ≤2MB 的 .csv 文件' })
      return
    }
    const form = new FormData()
    form.append('file', file)
    await call(async () => {
      const r = await api.post<{ data: { updated: number; backup: string } }>('/api/qydion/pricing/import', form)
      return r
    }, '已导入（导入前已自动备份当前倍率到 config/pricing-history/）')
  }
  return (
    <div className='space-y-3'>
      {status.msg && <p className={status.ok ? 'text-xs text-emerald-500' : 'text-xs text-red-500'}>{status.msg}</p>}
      <p className='text-muted-foreground text-xs'>
        {t('换算公式（已按源码核实）：模型倍率=输入价(元/百万)÷(2×Price)；补全倍率=输出价÷输入价；per_call 每次价(元)÷Price=美元/次。每次导入自动备份。')}
      </p>
      <div className='flex flex-wrap items-center gap-2'>
        <Button size='sm' variant='outline' onClick={() => downloadBlob('/api/qydion/pricing/export', 'qydion-pricing.csv')}>{t('导出当前价格表 CSV')}</Button>
        <input ref={fileRef} type='file' accept='.csv' className='text-xs' />
        <Button size='sm' onClick={doImport}>{t('导入 CSV（自动换算+备份）')}</Button>
      </div>
    </div>
  )
}

// ---------- 活动 ----------
function CampaignsPanel() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const { status, setStatus, call } = useQydionApi()
  const list = useQuery({
    queryKey: ['qydion-campaigns'],
    queryFn: async () => (await api.get<{ data: Array<Record<string, unknown>> }>('/api/qydion/campaigns')).data.data,
  })
  const [form, setForm] = useState({
    name: '', type: 'topup_bonus',
    rule: '{ "tiers": [ {"min": 100, "bonus": 10}, {"min": 500, "bonus": 80} ], "first_time_only": false, "bonus_expiry_days": 90 }',
    budget_quota: '0', per_user_limit: '0',
  })
  const create = async () => {
    await call(async () => {
      const now = new Date()
      const r = await api.post('/api/qydion/campaigns', {
        name: form.name, type: form.type, rule: form.rule,
        starts_at: now.toISOString(), ends_at: new Date(now.getTime() + 30 * 86400000).toISOString(),
        budget_quota: Number(form.budget_quota) || 0, per_user_limit: Number(form.per_user_limit) || 0,
      })
      return r
    }, '活动已创建（draft），请在列表中激活')
    qc.invalidateQueries({ queryKey: ['qydion-campaigns'] })
  }
  const setStatusOf = async (id: number, s: string) => {
    await call(() => api.post('/api/qydion/campaigns/status', { id, status: s }), '状态已更新')
    qc.invalidateQueries({ queryKey: ['qydion-campaigns'] })
  }
  return (
    <div className='space-y-3'>
      {status.msg && <p className={status.ok ? 'text-xs text-emerald-500' : 'text-xs text-red-500'}>{status.msg}</p>}
      <Card>
        <CardHeader><CardTitle className='text-sm'>{t('新建活动')}</CardTitle></CardHeader>
        <CardContent className='space-y-2'>
          <div className='flex flex-wrap gap-2'>
            <Input className='h-8 w-48' placeholder={t('活动名（如：618 充值满送）')} value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
            <select className='border-input bg-background h-8 rounded-md border text-xs' value={form.type} onChange={(e) => setForm({ ...form, type: e.target.value })}>
              <option value='topup_bonus'>充值满送</option>
              <option value='invite_boost'>返佣预算</option>
              <option value='new_user_gift'>新客礼包</option>
            </select>
            <Input className='h-8 w-28' placeholder='预算 quota(0=不限)' value={form.budget_quota} onChange={(e) => setForm({ ...form, budget_quota: e.target.value })} />
            <Input className='h-8 w-28' placeholder='每用户限次(0=不限)' value={form.per_user_limit} onChange={(e) => setForm({ ...form, per_user_limit: e.target.value })} />
            <Button size='sm' onClick={create}>{t('创建活动')}</Button>
          </div>
          <Textarea className='h-20 font-mono text-xs' value={form.rule} onChange={(e) => setForm({ ...form, rule: e.target.value })} />
        </CardContent>
      </Card>
      <Card>
        <CardContent className='pt-4'>
          <Table>
            <TableHeader><TableRow><TableHead>ID</TableHead><TableHead>名称</TableHead><TableHead>类型</TableHead><TableHead>状态</TableHead><TableHead>预算用量</TableHead><TableHead>操作</TableHead></TableRow></TableHeader>
            <TableBody>
              {(list.data ?? []).map((camp) => (
                <TableRow key={String(camp.id)}>
                  <TableCell>{String(camp.id)}</TableCell>
                  <TableCell>{String(camp.name)}</TableCell>
                  <TableCell><Badge variant='outline'>{String(camp.type)}</Badge></TableCell>
                  <TableCell>
                    <Badge variant={camp.status === 'active' ? 'default' : 'secondary'}>{String(camp.display_status ?? camp.status)}</Badge>
                  </TableCell>
                  <TableCell>{String(camp.used_quota)}/{String(camp.budget_quota || '∞')}</TableCell>
                  <TableCell className='flex gap-1'>
                    <Button size='sm' variant='outline' onClick={() => setStatusOf(Number(camp.id), 'active')}>{t('激活')}</Button>
                    <Button size='sm' variant='outline' onClick={() => setStatusOf(Number(camp.id), 'ended')}>{t('结束')}</Button>
                    <Button size='sm' variant='destructive' onClick={() => setStatusOf(Number(camp.id), 'terminated')}>{t('终止')}</Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}

// ---------- 优惠券 ----------
function CouponsPanel() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const { status, setStatus, call } = useQydionApi()
  const [batchId, setBatchId] = useState('')
  const [form, setForm] = useState({ count: '20', prefix: 'QYDION', type: 'amount', value: '10', min_amount: '0' })
  const [codes, setCodes] = useState<string[]>([])
  const list = useQuery({
    queryKey: ['qydion-coupons', batchId],
    queryFn: async () => batchId
      ? (await api.get<{ data: Array<Record<string, unknown>> }>(`/api/qydion/coupons?batch_id=${encodeURIComponent(batchId)}`)).data.data
      : [],
    enabled: batchId !== '',
  })
  const generate = async () => {
    await call(async () => {
      const r = await api.post<{ data: { codes: Array<{ code: string }>; batch_id: number } }>('/api/qydion/coupons/generate', {
        count: Number(form.count), prefix: form.prefix, type: form.type,
        value: Number(form.value), min_amount: Number(form.min_amount),
        expires_at: new Date(Date.now() + 90 * 86400000).toISOString(),
      })
      setCodes(r.data.data.codes.map((c) => c.code))
      setBatchId(String(r.data.data.batch_id))
      return r
    }, '批次已生成')
    qc.invalidateQueries({ queryKey: ['qydion-coupons'] })
  }
  const voidCoupon = async (id: number) => {
    await call(() => api.post('/api/qydion/coupons/void', { id }), '已作废')
    qc.invalidateQueries({ queryKey: ['qydion-coupons'] })
  }
  return (
    <div className='space-y-3'>
      {status.msg && <p className={status.ok ? 'text-xs text-emerald-500' : 'text-xs text-red-500'}>{status.msg}</p>}
      <Card>
        <CardHeader><CardTitle className='text-sm'>{t('批次生成')}</CardTitle></CardHeader>
        <CardContent className='flex flex-wrap gap-2'>
          <Input className='h-8 w-20' placeholder='数量' value={form.count} onChange={(e) => setForm({ ...form, count: e.target.value })} />
          <Input className='h-8 w-28' placeholder='前缀' value={form.prefix} onChange={(e) => setForm({ ...form, prefix: e.target.value })} />
          <select className='border-input bg-background h-8 rounded-md border text-xs' value={form.type} onChange={(e) => setForm({ ...form, type: e.target.value })}>
            <option value='amount'>满减(元)</option>
            <option value='discount'>折扣率(90=九折)</option>
          </select>
          <Input className='h-8 w-24' placeholder='面值' value={form.value} onChange={(e) => setForm({ ...form, value: e.target.value })} />
          <Input className='h-8 w-28' placeholder='使用门槛(元)' value={form.min_amount} onChange={(e) => setForm({ ...form, min_amount: e.target.value })} />
          <Button size='sm' onClick={generate}>{t('生成批次')}</Button>
          {batchId && (
            <Button size='sm' variant='outline' onClick={() => downloadBlob(`/api/qydion/coupons/export?batch_id=${encodeURIComponent(batchId)}`, `coupons-${batchId}.csv`)}>{t('导出 CSV')}</Button>
          )}
        </CardContent>
      </Card>
      {codes.length > 0 && (
        <Card><CardContent className='pt-4'><Textarea className='h-32 font-mono text-xs' readOnly value={codes.join('\n')} /></CardContent></Card>
      )}
      {batchId && (
        <Card>
          <CardContent className='pt-4'>
            <Table>
              <TableHeader><TableRow><TableHead>券码</TableHead><TableHead>类型</TableHead><TableHead>面值</TableHead><TableHead>状态</TableHead><TableHead></TableHead></TableRow></TableHeader>
              <TableBody>
                {(list.data ?? []).slice(0, 50).map((cp) => (
                  <TableRow key={String(cp.id)}>
                    <TableCell className='font-mono text-xs'>{String(cp.code)}</TableCell>
                    <TableCell>{String(cp.type)}</TableCell>
                    <TableCell>{String(cp.value)}</TableCell>
                    <TableCell><Badge variant={cp.status === 'unused' ? 'default' : 'secondary'}>{String(cp.status)}</Badge></TableCell>
                    <TableCell>{cp.status === 'unused' && <Button size='sm' variant='destructive' onClick={() => voidCoupon(Number(cp.id))}>{t('作废')}</Button>}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}
    </div>
  )
}

// ---------- 对公审核 ----------
function CorporatePanel() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const { status, setStatus, call } = useQydionApi()
  const [reasons, setReasons] = useState<Record<string, string>>({})
  const queue = useQuery({
    queryKey: ['qydion-review-queue'],
    queryFn: async () => (await api.get<{ data: Array<Record<string, unknown>> }>('/api/qydion/topup/review-queue')).data.data,
    refetchInterval: 15000,
  })
  const review = async (tradeNo: string, action: 'approve' | 'reject') => {
    await call(
      () => api.post('/api/qydion/topup/review', { trade_no: tradeNo, action, reason: reasons[tradeNo] ?? '' }),
      action === 'approve' ? '已通过并入账' : '已驳回',
    )
    qc.invalidateQueries({ queryKey: ['qydion-review-queue'] })
  }
  return (
    <div className='space-y-3'>
      {status.msg && <p className={status.ok ? 'text-xs text-emerald-500' : 'text-xs text-red-500'}>{status.msg}</p>}
      <Card>
        <CardHeader><CardTitle className='text-sm'>{t('充值审核队列（对公）')}</CardTitle></CardHeader>
        <CardContent>
          <Table>
            <TableHeader><TableRow><TableHead>订单号</TableHead><TableHead>用户</TableHead><TableHead>金额(元)</TableHead><TableHead>流水后6位</TableHead><TableHead>凭证</TableHead><TableHead>历史充值</TableHead><TableHead>操作</TableHead></TableRow></TableHeader>
            <TableBody>
              {(queue.data ?? []).map((item) => {
                const tradeNo = String(item.trade_no)
                return (
                  <TableRow key={tradeNo}>
                    <TableCell className='font-mono text-xs'>{tradeNo}</TableCell>
                    <TableCell>{String(item.username)}</TableCell>
                    <TableCell>{String(item.money)}</TableCell>
                    <TableCell>{String(item.serial_suffix)}</TableCell>
                    <TableCell>
                      <Button size='sm' variant='outline' onClick={() => downloadBlob(`/api/qydion/voucher/${encodeURIComponent(tradeNo)}`, `${tradeNo}-voucher`)}>{String(item.voucher_name ?? t('下载'))}</Button>
                    </TableCell>
                    <TableCell>{String(item.topup_count)} 次</TableCell>
                    <TableCell className='flex flex-wrap items-center gap-1'>
                      <Input className='h-8 w-40' placeholder={t('驳回原因（必填）')} value={reasons[tradeNo] ?? ''} onChange={(e) => setReasons({ ...reasons, [tradeNo]: e.target.value })} />
                      <Button size='sm' onClick={() => review(tradeNo, 'approve')}>{t('通过并入账')}</Button>
                      <Button size='sm' variant='destructive' onClick={() => review(tradeNo, 'reject')}>{t('驳回')}</Button>
                    </TableCell>
                  </TableRow>
                )
              })}
              {(queue.data ?? []).length === 0 && <TableRow><TableCell colSpan={7} className='text-muted-foreground text-center text-sm'>暂无待审核订单</TableCell></TableRow>}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}

// ---------- 对账 ----------
function ReconciliationPanel() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const { status, setStatus, call } = useQydionApi()
  const list = useQuery({
    queryKey: ['qydion-recon'],
    queryFn: async () => (await api.get<{ data: Array<Record<string, unknown>> }>('/api/qydion/reconciliation')).data.data,
  })
  const run = async () => {
    await call(() => api.post('/api/qydion/reconciliation/run', {}), '对账已执行')
    qc.invalidateQueries({ queryKey: ['qydion-recon'] })
  }
  return (
    <div className='space-y-3'>
      {status.msg && <p className={status.ok ? 'text-xs text-emerald-500' : 'text-xs text-red-500'}>{status.msg}</p>}
      <div className='flex items-center gap-2'>
        <Button size='sm' onClick={run}>{t('手动执行昨日对账')}</Button>
        <span className='text-muted-foreground text-xs'>{t('每日 03:00（Asia/Shanghai）自动执行；上游账单契约就绪前结果标记 no_upstream。')}</span>
      </div>
      <Card>
        <CardContent className='pt-4'>
          <Table>
            <TableHeader><TableRow><TableHead>日期</TableHead><TableHead>渠道</TableHead><TableHead>模型</TableHead><TableHead>本地tokens</TableHead><TableHead>上游tokens</TableHead><TableHead>差异%</TableHead><TableHead>状态</TableHead></TableRow></TableHeader>
            <TableBody>
              {(list.data ?? []).slice(0, 50).map((r, i) => (
                <TableRow key={i} className={r.status === 'pending' ? 'bg-amber-500/10' : ''}>
                  <TableCell>{String(r.date)}</TableCell>
                  <TableCell>{String(r.channel_id)}</TableCell>
                  <TableCell className='font-mono text-xs'>{String(r.model)}</TableCell>
                  <TableCell>{String(r.local_tokens)}</TableCell>
                  <TableCell>{String(r.upstream_tokens)}</TableCell>
                  <TableCell>{Number(r.diff_ratio).toFixed(2)}</TableCell>
                  <TableCell><Badge variant={r.status === 'ok' ? 'default' : r.status === 'pending' ? 'destructive' : 'secondary'}>{String(r.status)}</Badge></TableCell>
                </TableRow>
              ))}
              {(list.data ?? []).length === 0 && <TableRow><TableCell colSpan={7} className='text-muted-foreground text-center text-sm'>暂无对账数据</TableCell></TableRow>}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  )
}

export default function QydionConsole() {
  const { t } = useTranslation()
  return (
    <div className='mx-auto w-full max-w-6xl px-4 py-6'>
      <h1 className='mb-4 text-xl font-semibold'>{t('QYDION 运营台')}</h1>
      <Tabs defaultValue='stats'>
        <TabsList className='flex flex-wrap'>
          <TabsTrigger value='stats'>{t('统计')}</TabsTrigger>
          <TabsTrigger value='models'>{t('模型维护')}</TabsTrigger>
          <TabsTrigger value='pricing'>{t('价格表')}</TabsTrigger>
          <TabsTrigger value='campaigns'>{t('活动管理')}</TabsTrigger>
          <TabsTrigger value='coupons'>{t('优惠券')}</TabsTrigger>
          <TabsTrigger value='corporate'>{t('充值审核')}</TabsTrigger>
          <TabsTrigger value='recon'>{t('对账中心')}</TabsTrigger>
          <TabsTrigger value='paysettings'>{t('支付设置')}</TabsTrigger>
        </TabsList>
        <TabsContent value='stats'><StatsPanel /></TabsContent>
        <TabsContent value='models'><ModelExtPanel /></TabsContent>
        <TabsContent value='pricing'><PricingPanel /></TabsContent>
        <TabsContent value='campaigns'><CampaignsPanel /></TabsContent>
        <TabsContent value='coupons'><CouponsPanel /></TabsContent>
        <TabsContent value='corporate'><CorporatePanel /></TabsContent>
        <TabsContent value='recon'><ReconciliationPanel /></TabsContent>
        <TabsContent value='paysettings'><PaymentSettingsPanel /></TabsContent>
      </Tabs>
    </div>
  )
}

// ---------- 支付设置（M7 §8.6b：填参数即启用） ----------
const PAY_SETTING_FIELDS: Array<{ group: string; items: Array<{ key: string; label: string; placeholder: string; secret?: boolean }> }> = [
  {
    group: '支付宝当面付（扫码）',
    items: [
      { key: 'QydionAlipayAppId', label: 'AppID（应用 ID）', placeholder: '例如 2021001234567890' },
      { key: 'QydionAlipayPrivateKey', label: '应用私钥（PKCS1/PKCS8）', placeholder: '-----BEGIN RSA PRIVATE KEY----- …', secret: true },
      { key: 'QydionAlipayPublicKey', label: '支付宝公钥（非应用公钥）', placeholder: '-----BEGIN PUBLIC KEY----- …', secret: true },
      { key: 'QydionAlipayGateway', label: '网关（沙箱可填沙箱地址）', placeholder: 'https://openapi.alipay.com/gateway.do' },
    ],
  },
  {
    group: '微信 Native 扫码',
    items: [
      { key: 'QydionWxpayAppId', label: 'AppID（公众号/应用）', placeholder: 'wx开头' },
      { key: 'QydionWxpayMchId', label: '商户号 mchid', placeholder: '例如 1900000109' },
      { key: 'QydionWxpayMchSerial', label: '商户 API 证书序列号', placeholder: '例如 5157F09EFDC096DE15EBE81A47057A72…' },
      { key: 'QydionWxpayPrivateKey', label: '商户 API 私钥（apiclient_key.pem 内容）', placeholder: '-----BEGIN PRIVATE KEY----- …', secret: true },
      { key: 'QydionWxpayApiV3Key', label: 'APIv3 密钥（32 位）', placeholder: '32 位字符串', secret: true },
    ],
  },
  {
    group: 'Stripe Checkout（国际卡支付）',
    items: [
      { key: 'QydionStripeSecretKey', label: 'Secret Key', placeholder: 'sk_live_… / sk_test_…', secret: true },
      { key: 'QydionStripeWebhookSecret', label: 'Webhook 签名密钥', placeholder: 'whsec_…', secret: true },
      { key: 'QydionStripeCurrency', label: '计价币种（默认 usd）', placeholder: 'usd' },
      { key: 'QydionStripeSuccessUrl', label: '支付完成回跳 URL（留空用站点 /console）', placeholder: 'https://…/console' },
      { key: 'QydionStripeCancelUrl', label: '取消支付回跳 URL（留空用站点 /console）', placeholder: 'https://…/console' },
    ],
  },
]

function PaymentSettingsPanel() {
  const { t } = useTranslation()
  const { status, setStatus, call } = useQydionApi()
  const qc = useQueryClient()
  const [draft, setDraft] = useState<Record<string, string>>({})
  const saved = useQuery({
    queryKey: ['qydion-pay-settings'],
    queryFn: async () => (await api.get<{ data: Record<string, string> }>('/api/qydion/payment/settings')).data.data,
  })
  const save = async (updates: Record<string, string>) => {
    const ok = await call(async () => {
      const r = await api.post('/api/qydion/payment/settings', updates)
      return r
    }, '已保存（即时生效）')
    if (ok) {
      setDraft({})
      qc.invalidateQueries({ queryKey: ['qydion-pay-settings'] })
      qc.invalidateQueries({ queryKey: ['qydion-pay-methods'] })
    }
  }
  return (
    <div className='space-y-4'>
      {status.msg && <p className={status.ok ? 'text-xs text-emerald-500' : 'text-xs text-red-500'}>{status.msg}</p>}
      <p className='text-muted-foreground text-xs'>
        {t('回调地址（需在渠道后台配置）：支付宝=服务器异步通知 URL；微信=Native 回调 URL；Stripe=Webhook endpoint。格式：https://你的域名/api/qydion/payment/notify/渠道名（alipay_f2f / wx_native / stripe）。密钥只保存一次，读取只显示尾号；留空提交=清空该项。')}
      </p>
      {PAY_SETTING_FIELDS.map((g) => (
        <Card key={g.group}>
          <CardHeader><CardTitle className='text-sm'>{t(g.group)}</CardTitle></CardHeader>
          <CardContent className='space-y-2'>
            {g.items.map((f) => {
              const cur = saved.data?.[f.key] ?? ''
              const val = draft[f.key]
              return (
                <div key={f.key} className='flex flex-wrap items-center gap-2'>
                  <span className='w-64 shrink-0 text-xs'>{t(f.label)}</span>
                  <Input
                    className='h-8 flex-1 min-w-56 font-mono text-xs'
                    placeholder={f.secret ? (cur ? cur : f.placeholder) : (cur || f.placeholder)}
                    value={val ?? ''}
                    onChange={(e) => setDraft({ ...draft, [f.key]: e.target.value })}
                  />
                </div>
              )
            })}
            <Button size='sm' disabled={Object.keys(draft).length === 0} onClick={() => save(draft)}>
              {t('保存本渠道修改')}
            </Button>
          </CardContent>
        </Card>
      ))}
    </div>
  )
}
