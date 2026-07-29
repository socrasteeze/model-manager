import { useEffect, useState } from 'react'

interface Props {
  label: string
  value?: string
  editable: boolean
  multiline?: boolean
  options?: string[]
  onSave: (value: string | null) => void
}

export function EditableField({ label, value, editable, multiline, options, onSave }: Props) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(value ?? '')

  useEffect(() => setDraft(value ?? ''), [value])

  if (!editing) {
    if (!value && !editable) return null
    return (
      <div className="row">
        <span className="row-label">{label}</span>
        <span className="row-value">
          {value || <em className="unset">not set</em>}
          {editable && (
            <button className="edit" onClick={() => setEditing(true)} aria-label={`Edit ${label}`}>
              edit
            </button>
          )}
        </span>
      </div>
    )
  }

  const commit = () => {
    const trimmed = draft.trim()
    // An empty submission clears the manual value so lower-tier sources resolve
    // again, rather than storing an empty string that would stick forever.
    onSave(trimmed === '' ? null : trimmed)
    setEditing(false)
  }

  return (
    <div className="row editing">
      <span className="row-label">{label}</span>
      <div className="row-value">
        {options ? (
          <select value={draft} onChange={(e) => setDraft(e.target.value)} autoFocus>
            <option value="">(not set)</option>
            {options.map((o) => (
              <option key={o} value={o}>
                {o}
              </option>
            ))}
          </select>
        ) : multiline ? (
          <textarea value={draft} onChange={(e) => setDraft(e.target.value)} rows={4} autoFocus />
        ) : (
          <input
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') commit()
              if (e.key === 'Escape') {
                setDraft(value ?? '')
                setEditing(false)
              }
            }}
            autoFocus
          />
        )}
        <div className="edit-actions">
          <button onClick={commit}>Save</button>
          <button
            onClick={() => {
              setDraft(value ?? '')
              setEditing(false)
            }}
          >
            Cancel
          </button>
        </div>
      </div>
    </div>
  )
}
