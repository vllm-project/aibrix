import type { CSSProperties, ReactNode } from 'react'

interface TooltipProps {
  content: string
  children: ReactNode
  position?: 'top' | 'bottom'
  delay?: number
}

export function Tooltip({ content, children, position = 'top', delay = 400 }: TooltipProps) {
  return (
    <span className="relative inline-flex group" style={{ '--tooltip-delay': `${delay}ms` } as CSSProperties}>
      {children}
      <span
        className={`absolute left-1/2 -translate-x-1/2 z-[300] px-2.5 py-1 rounded-lg bg-foreground text-background text-xs whitespace-nowrap pointer-events-none opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 transition-opacity delay-0 group-hover:delay-[var(--tooltip-delay)] group-focus-within:delay-[var(--tooltip-delay)] ${
          position === 'top' ? 'bottom-full mb-2' : 'top-full mt-2'
        }`}
      >
        {content}
      </span>
    </span>
  )
}
