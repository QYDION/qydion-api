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
import { type SVGProps } from 'react'

import { cn } from '@/lib/utils'

export function Logo({ className, ...props }: SVGProps<SVGSVGElement>) {
  return (
    <svg
      id='qydion-logo'
      viewBox='-50 -50 109.788 122.978'
      xmlns='http://www.w3.org/2000/svg'
      height='24'
      width='24'
      fill='currentColor'
      className={cn('size-6', className)}
      {...props}
    >
      <title>QYDION API</title>
      <path d='M22.821 44.488L59.788 57.943L54.316 72.978L-17.101 46.985A50 50 0 1 1 22.821 44.488ZM34 0A34 34 0 1 0 -34 0A34 34 0 1 0 34 0Z' />
    </svg>
  )
}
