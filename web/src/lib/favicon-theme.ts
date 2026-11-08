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
import { applyFaviconToDom } from '@/lib/dom-utils'

/*
 * QYDION：标签页图标 = 官方小号图形（无底，final/svg/qydion-symbol-small-*）。
 * 不用 SVG 内的 prefers-color-scheme 媒体查询——实测它跟随**系统主题**而非浏览器
 * 外观设置，"系统浅色 + 浏览器深色"时会渲染 Ink 黑标，融进深色标签栏不可见。
 * 这里改由页面 JS 用 matchMedia 判定（页面上下文的 matchMedia 跟随浏览器外观，
 * 即决定标签栏颜色的那一层），主题变化实时跟随。
 */
/* 与官网 rebuild-cn 同名同字节资产（final/icons/favicon.svg 几何的固定填色版） */
const FAVICON_DARK = '/favicon-dark.svg' // 深色标签栏 → 白色图形
const FAVICON_LIGHT = '/favicon-light.svg' // 浅色标签栏 → Ink 图形

export function isQydionFavicon(url: string | null | undefined) {
  return (
    url === FAVICON_DARK ||
    url === FAVICON_LIGHT ||
    url === FAVICON_DARK + '?' ||
    url === FAVICON_LIGHT + '?'
  )
}

export function applyBrowserThemeFavicon() {
  if (typeof window === 'undefined' || !window.matchMedia) return
  const dark = window.matchMedia('(prefers-color-scheme: dark)').matches
  applyFaviconToDom(dark ? FAVICON_DARK : FAVICON_LIGHT)
}

/** 首帧调用：立即按浏览器主题放置图标，并注册主题变化跟随。 */
export function initBrowserThemeFavicon() {
  applyBrowserThemeFavicon()
  if (typeof window === 'undefined' || !window.matchMedia) return
  const mq = window.matchMedia('(prefers-color-scheme: dark)')
  mq.addEventListener('change', () => {
    // 管理员自定义 logo 接管后不再抢写
    const current = document
      .querySelector('link[rel~="icon"]')
      ?.getAttribute('href')
    if (!isQydionFavicon(current)) return
    applyBrowserThemeFavicon()
  })
}
