import { useState } from 'react'

interface Props {
  value: string
  label?: string
  className?: string
}

export function CopyButton({ value, label, className }: Props) {
  const [copied, setCopied] = useState(false)

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
    } catch {
      // navigator.clipboard is unavailable over plain HTTP on some browsers,
      // which is exactly how this app is reached over a tailnet. Fall back to
      // the old selection trick rather than silently doing nothing.
      const el = document.createElement('textarea')
      el.value = value
      el.setAttribute('readonly', '')
      el.style.position = 'fixed'
      el.style.opacity = '0'
      document.body.appendChild(el)
      el.select()
      try {
        document.execCommand('copy')
      } finally {
        document.body.removeChild(el)
      }
    }
    setCopied(true)
    setTimeout(() => setCopied(false), 1200)
  }

  return (
    <button className={`copy${className ? ` ${className}` : ''}`} onClick={copy} title="Copy">
      {copied ? 'copied' : (label ?? value)}
    </button>
  )
}
