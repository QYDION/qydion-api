/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { zodResolver } from '@hookform/resolvers/zod'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import i18next from 'i18next'
import type { Resolver } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import * as z from 'zod'

import { api } from '@/lib/api'
import { handleServerError } from '@/lib/handle-server-error'
import { requireServerSuccess } from '@/lib/server-error-message'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'

import { FormDirtyIndicator } from '../components/form-dirty-indicator'
import { FormNavigationGuard } from '../components/form-navigation-guard'
import {
  SettingsForm,
  SettingsFormGrid,
  SettingsFormGridItem,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useSettingsForm } from '../hooks/use-settings-form'

// QYDION 运营设置键（与后端 service/qydion_operation_settings.go 白名单一一对应）。
// 类型/默认值以后端读取调用为准：service/qydion_rebate.go、qydion_guards.go、
// controller/qydion_tasks.go、qydion_reconciliation.go、payment/corporate.go、service/qydion_alerts.go。
const QYDION_OPS_DEFAULTS = {
  QydionRebateEnabled: false,
  QydionRebateFirstRate: 10,
  QydionRebateSecondRate: 0,
  QydionRebateMinSettleCNY: 10,
  QydionMaxTokensLimit: 8192,
  QydionDailySpendAlertCNY: 500,
  QydionAutoDisableOnDebt: true,
  QydionCorpAccountName: '',
  QydionCorpAccountNo: '',
  QydionCorpBankName: '',
  QydionCorpNote: '',
  QydionEstimatedUsageWarn: 1,
  QydionReconDiffTokens: 2,
  QydionReconDiffAmount: 2,
  QydionCorporateEnabled: false,
  QydionAlertEmail: '',
} as const

type QydionOpsFormValues = {
  [K in keyof typeof QYDION_OPS_DEFAULTS]: (typeof QYDION_OPS_DEFAULTS)[K] extends boolean
    ? boolean
    : (typeof QYDION_OPS_DEFAULTS)[K] extends number
      ? number
      : string
}

const FIELD_LABELS: Record<keyof QydionOpsFormValues, string> = {
  QydionRebateEnabled: 'Enable referral rebate',
  QydionRebateFirstRate: 'Level 1 rebate rate (%)',
  QydionRebateSecondRate: 'Level 2 rebate rate (%)',
  QydionRebateMinSettleCNY: 'Minimum settlement amount (CNY)',
  QydionMaxTokensLimit: 'Single-request max_tokens clamp',
  QydionDailySpendAlertCNY: 'Daily spend alert threshold (CNY)',
  QydionAutoDisableOnDebt: 'Auto-disable token on negative balance',
  QydionCorpAccountName: 'Corporate account name',
  QydionCorpAccountNo: 'Corporate account number',
  QydionCorpBankName: 'Corporate bank name',
  QydionCorpNote: 'Corporate transfer note',
  QydionEstimatedUsageWarn: 'Estimated usage alert threshold (%)',
  QydionReconDiffTokens: 'Reconciliation token diff threshold (%)',
  QydionReconDiffAmount: 'Reconciliation amount diff threshold (%)',
  QydionCorporateEnabled: 'Enable corporate transfer top-up',
  QydionAlertEmail: 'Admin alert email',
}

const FIELD_DESCRIPTIONS: Record<keyof QydionOpsFormValues, string> = {
  QydionRebateEnabled: 'Enable after configuring rebate rates; disabled by default',
  QydionRebateFirstRate: 'Default 10, range 0-100',
  QydionRebateSecondRate: 'Default 0 (level 2 rebate disabled), range 0-100',
  QydionRebateMinSettleCNY: 'Default 10; settlement only when reached (CNY)',
  QydionMaxTokensLimit: 'Default 8192; 0 disables the clamp',
  QydionDailySpendAlertCNY: 'Default 500; alert when a user exceeds this daily spend (CNY)',
  QydionAutoDisableOnDebt: 'Default enabled; auto-disable tokens with negative balance',
  QydionCorpAccountName: 'Shown on the corporate transfer top-up page',
  QydionCorpAccountNo: 'Shown on the corporate transfer top-up page',
  QydionCorpBankName: 'Shown on the corporate transfer top-up page',
  QydionCorpNote: 'Shown on the corporate transfer top-up page',
  QydionEstimatedUsageWarn: 'Default 1, range 0-100',
  QydionReconDiffTokens: 'Default 2, range 0-100',
  QydionReconDiffAmount: 'Default 2, range 0-100',
  QydionCorporateEnabled: 'Default off; also requires payment compliance confirmation',
  QydionAlertEmail: 'Empty = log-only alerts; set to enable email alerts via SMTP',
}

const rateField = () =>
  z
    .number({ error: () => i18nT('Must be a number') })
    .min(0, { error: () => i18nT('Must be between 0 and 100') })
    .max(100, { error: () => i18nT('Must be between 0 and 100') })

const nonNegativeField = () =>
  z.number({ error: () => i18nT('Must be a number') }).min(0, {
    error: () => i18nT('Must be a non-negative number'),
  })

// zod error message 在模块层定义，无法用 useTranslation hook，这里借 i18next 实例。
function i18nT(key: string) {
  return i18next.t(key)
}

function buildQydionOpsSchema() {
  return z.object({
    QydionRebateEnabled: z.boolean(),
    QydionRebateFirstRate: rateField(),
    QydionRebateSecondRate: rateField(),
    QydionRebateMinSettleCNY: nonNegativeField(),
    QydionMaxTokensLimit: nonNegativeField(),
    QydionDailySpendAlertCNY: nonNegativeField(),
    QydionAutoDisableOnDebt: z.boolean(),
    QydionCorpAccountName: z.string(),
    QydionCorpAccountNo: z.string(),
    QydionCorpBankName: z.string(),
    QydionCorpNote: z.string(),
    QydionEstimatedUsageWarn: rateField(),
    QydionReconDiffTokens: rateField(),
    QydionReconDiffAmount: rateField(),
    QydionCorporateEnabled: z.boolean(),
    QydionAlertEmail: z
      .string()
      .refine(
        (v) => v === '' || z.string().email().safeParse(v).success,
        { error: () => i18nT('Enter a valid email address or leave empty') }
      ),
  })
}

function parseSavedNumber(
  saved: Record<string, string> | undefined,
  key: keyof typeof QYDION_OPS_DEFAULTS,
  fallback: number
): number {
  const raw = saved?.[key]
  if (raw === undefined || raw.trim() === '') return fallback
  const n = Number(raw)
  return Number.isNaN(n) ? fallback : n
}

function parseSavedBool(
  saved: Record<string, string> | undefined,
  key: keyof typeof QYDION_OPS_DEFAULTS,
  fallback: boolean
): boolean {
  const raw = saved?.[key]
  if (raw === undefined || raw === '') return fallback
  return raw === 'true' || raw === '1'
}

function parseSavedString(
  saved: Record<string, string> | undefined,
  key: keyof typeof QYDION_OPS_DEFAULTS,
  fallback: string
): string {
  return saved?.[key] ?? fallback
}

export function QydionOperationsSection() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()

  const saved = useQuery({
    queryKey: ['qydion-pay-settings'],
    queryFn: async () =>
      (
        await api.get<{ data: Record<string, string> }>(
          '/api/qydion/payment/settings'
        )
      ).data.data,
  })

  if (saved.isLoading || saved.data === undefined) {
    return (
      <SettingsSection title={t('QYDION Operations')}>
        <div className='text-muted-foreground flex min-h-24 items-center justify-center text-sm'>
          {saved.isError
            ? t('Failed to load settings')
            : t('Loading QYDION operations settings...')}
        </div>
      </SettingsSection>
    )
  }

  return (
    <QydionOpsForm
      saved={saved.data}
      onSaved={() =>
        queryClient.invalidateQueries({ queryKey: ['qydion-pay-settings'] })
      }
    />
  )
}

function QydionOpsForm({
  saved,
  onSaved,
}: {
  saved: Record<string, string>
  onSaved: () => void
}) {
  const { t } = useTranslation()

  const defaultValues: QydionOpsFormValues = {
    QydionRebateEnabled: parseSavedBool(saved, 'QydionRebateEnabled', false),
    QydionRebateFirstRate: parseSavedNumber(
      saved,
      'QydionRebateFirstRate',
      QYDION_OPS_DEFAULTS.QydionRebateFirstRate
    ),
    QydionRebateSecondRate: parseSavedNumber(
      saved,
      'QydionRebateSecondRate',
      QYDION_OPS_DEFAULTS.QydionRebateSecondRate
    ),
    QydionRebateMinSettleCNY: parseSavedNumber(
      saved,
      'QydionRebateMinSettleCNY',
      QYDION_OPS_DEFAULTS.QydionRebateMinSettleCNY
    ),
    QydionMaxTokensLimit: parseSavedNumber(
      saved,
      'QydionMaxTokensLimit',
      QYDION_OPS_DEFAULTS.QydionMaxTokensLimit
    ),
    QydionDailySpendAlertCNY: parseSavedNumber(
      saved,
      'QydionDailySpendAlertCNY',
      QYDION_OPS_DEFAULTS.QydionDailySpendAlertCNY
    ),
    QydionAutoDisableOnDebt: parseSavedBool(
      saved,
      'QydionAutoDisableOnDebt',
      true
    ),
    QydionCorpAccountName: parseSavedString(
      saved,
      'QydionCorpAccountName',
      ''
    ),
    QydionCorpAccountNo: parseSavedString(saved, 'QydionCorpAccountNo', ''),
    QydionCorpBankName: parseSavedString(saved, 'QydionCorpBankName', ''),
    QydionCorpNote: parseSavedString(saved, 'QydionCorpNote', ''),
    QydionEstimatedUsageWarn: parseSavedNumber(
      saved,
      'QydionEstimatedUsageWarn',
      QYDION_OPS_DEFAULTS.QydionEstimatedUsageWarn
    ),
    QydionReconDiffTokens: parseSavedNumber(
      saved,
      'QydionReconDiffTokens',
      QYDION_OPS_DEFAULTS.QydionReconDiffTokens
    ),
    QydionReconDiffAmount: parseSavedNumber(
      saved,
      'QydionReconDiffAmount',
      QYDION_OPS_DEFAULTS.QydionReconDiffAmount
    ),
    QydionCorporateEnabled: parseSavedBool(
      saved,
      'QydionCorporateEnabled',
      false
    ),
    QydionAlertEmail: parseSavedString(saved, 'QydionAlertEmail', ''),
  }

  const qydionOpsSchema = buildQydionOpsSchema()

  const { form, handleSubmit, handleReset, isDirty, isSubmitting } =
    useSettingsForm<QydionOpsFormValues>({
      resolver: zodResolver(qydionOpsSchema) as Resolver<
        QydionOpsFormValues,
        unknown,
        QydionOpsFormValues
      >,
      defaultValues,
      onSubmit: async (_data, changedFields) => {
        // 保存走既有支付设置端点（POST /api/qydion/payment/settings），
        // 后端白名单见 service/qydion_operation_settings.go；布尔/数值转字符串后逐键提交。
        const updates: Record<string, string> = {}
        for (const [key, value] of Object.entries(changedFields)) {
          if (typeof value === 'boolean') {
            updates[key] = value ? 'true' : 'false'
          } else {
            updates[key] = String(value)
          }
        }
        try {
          await requireServerSuccess(
            (await api.post<{ success: boolean }>(
              '/api/qydion/payment/settings',
              updates
            )).data
          )
          toast.success(t('QYDION operations settings saved'))
          onSaved()
        } catch (error) {
          handleServerError(error, t('Failed to save settings'))
        }
      },
    })

  const renderSwitch = (name: keyof QydionOpsFormValues) => (
    <FormField
      control={form.control}
      name={name}
      render={({ field }) => (
        <SettingsSwitchItem>
          <SettingsSwitchContent>
            <FormLabel>{t(FIELD_LABELS[name])}</FormLabel>
            <FormDescription>{t(FIELD_DESCRIPTIONS[name])}</FormDescription>
          </SettingsSwitchContent>
          <FormControl>
            <Switch
              checked={field.value as boolean}
              onCheckedChange={field.onChange}
            />
          </FormControl>
        </SettingsSwitchItem>
      )}
    />
  )

  const renderNumber = (
    name: keyof QydionOpsFormValues,
    placeholder: string
  ) => (
    <FormField
      control={form.control}
      name={name}
      render={({ field }) => (
        <FormItem>
          <FormLabel>{t(FIELD_LABELS[name])}</FormLabel>
          <FormControl>
            <Input
              type='number'
              step='any'
              min={0}
              placeholder={placeholder}
              {...field}
              value={String(field.value ?? '')}
              onChange={(e) =>
                field.onChange(
                  e.target.value === '' ? 0 : Number(e.target.value)
                )
              }
            />
          </FormControl>
          <FormDescription>
            {t(FIELD_DESCRIPTIONS[name])}
          </FormDescription>
          <FormMessage />
        </FormItem>
      )}
    />
  )

  const renderText = (name: keyof QydionOpsFormValues) => (
    <FormField
      control={form.control}
      name={name}
      render={({ field }) => (
        <FormItem>
          <FormLabel>{t(FIELD_LABELS[name])}</FormLabel>
          <FormControl>
            <Input {...field} value={String(field.value ?? '')} />
          </FormControl>
          <FormDescription>{t(FIELD_DESCRIPTIONS[name])}</FormDescription>
          <FormMessage />
        </FormItem>
      )}
    />
  )

  return (
    <>
      <FormNavigationGuard when={isDirty} />

      <SettingsSection title={t('QYDION Operations')}>
        <Form {...form}>
          <SettingsForm onSubmit={handleSubmit}>
            <SettingsPageFormActions
              onSave={handleSubmit}
              onReset={handleReset}
              isSaving={isSubmitting}
              isResetDisabled={!isDirty}
            />
            <FormDirtyIndicator isDirty={isDirty} />
            <SettingsFormGrid>
              {renderSwitch('QydionRebateEnabled')}
              {renderNumber('QydionRebateFirstRate', '10')}
              {renderNumber('QydionRebateSecondRate', '0')}
              {renderNumber('QydionRebateMinSettleCNY', '10')}
              {renderNumber('QydionMaxTokensLimit', '8192')}
              {renderNumber('QydionDailySpendAlertCNY', '500')}
              {renderSwitch('QydionAutoDisableOnDebt')}
              {renderText('QydionCorpAccountName')}
              {renderText('QydionCorpAccountNo')}
              {renderText('QydionCorpBankName')}
              {renderText('QydionCorpNote')}
              <SettingsFormGridItem span='full'>
                {renderText('QydionAlertEmail')}
              </SettingsFormGridItem>
              {renderNumber('QydionEstimatedUsageWarn', '1')}
              {renderNumber('QydionReconDiffTokens', '2')}
              {renderNumber('QydionReconDiffAmount', '2')}
              {renderSwitch('QydionCorporateEnabled')}
            </SettingsFormGrid>
          </SettingsForm>
        </Form>
      </SettingsSection>
    </>
  )
}
