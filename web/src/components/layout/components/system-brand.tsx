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
import { Link } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import {
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from '@/components/ui/sidebar'
import { useStatus } from '@/hooks/use-status'
import { cn } from '@/lib/utils'

type SystemBrandProps = {
  defaultName?: string
  defaultVersion?: string
  /**
   * Visual layout:
   * - 'sidebar': stacked card style (used inside the sidebar header).
   * - 'inline': compact horizontal pill (used inside the top app bar).
   */
  variant?: 'sidebar' | 'inline'
}

/**
 * System brand component
 * Displays current system logo + name.
 * - inline: compact pill in the top app bar; clicking navigates to home (/)
 * - sidebar: stacked card in the sidebar header (display only)
 */
export function SystemBrand(props: SystemBrandProps) {
  const { t } = useTranslation()
  const { status } = useStatus()

  const variant = props.variant ?? 'sidebar'
  const name = status?.system_name || props.defaultName || 'QYDION API'
  const version =
    status?.version || props.defaultVersion || t('Unknown version')

  if (variant === 'inline') {
    // QYDION：页头用官方横版组合 logo（规格书 §6.4，深底白字标版 06-horizontal.svg）
    return (
      <Link
        to='/'
        aria-label={`${name} — ${t('Go to home')}`}
        className={cn(
          'inline-flex h-7 min-w-0 items-center rounded-md px-1.5 transition-colors outline-none select-none',
          'hover:bg-accent focus-visible:ring-ring/40 focus-visible:ring-2'
        )}
      >
        <img
          src='/logo-wordmark.svg'
          alt={name}
          /* QYDION：主标志=字标本体（规范 §1/§8：图形不与字标横排）。
             48px 页头 → 字高≈13px（§6 比例）；上移补偿尾巴下垂的光学居中 */
          className='h-4 w-auto translate-y-[1.5px]'
        />
      </Link>
    )
  }

  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <SidebarMenuButton
          size='lg'
          className='hover:text-sidebar-foreground active:text-sidebar-foreground cursor-default hover:bg-transparent active:bg-transparent'
          render={<div />}
        >
          {/* QYDION：侧栏同用横版组合 logo，版本号保留在下方 */}
          <div className='flex min-w-0 flex-1 flex-col items-start gap-1 group-data-[collapsible=icon]:hidden'>
            <img
              src='/logo-wordmark.svg'
              alt={name}
              className='h-5 w-auto translate-y-[2px]'
            />
            <span className='truncate text-xs text-muted-foreground'>{version}</span>
          </div>
        </SidebarMenuButton>
      </SidebarMenuItem>
    </SidebarMenu>
  )
}
