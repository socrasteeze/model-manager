interface Props {
  label: string
  checked: boolean
  disabled?: boolean
  onChange: (next: boolean) => void

  /** Shown under the row, in the same voice as the panel's other hints. */
  hint?: string
}

/**
 * A boolean setting row.
 *
 * The first toggle in the app, so it sets the pattern. Two decisions worth
 * naming, because both differ from how every other control in SettingsPanel
 * behaves:
 *
 * It persists on change, not on blur. The text settings here save onBlur
 * because typing is not a decision until you leave the field. A checkbox click
 * *is* the decision, and waiting for a blur that a keyboard user may never
 * produce would quietly lose it.
 *
 * And the caller writes the value explicitly rather than deleting the key when
 * it matches the default. The tempting symmetry with the text fields -- blank
 * means deleteSetting, which restores the built-in default -- is wrong for a
 * boolean whose default is on: "never touched" and "deliberately switched back
 * on" would become the same stored state, so changing the default later would
 * silently reverse a choice the user actually made.
 *
 * Label first, control second, matching the rest of the panel so every control
 * lines up in one column.
 */
export function ToggleRow({ label, checked, disabled, onChange, hint }: Props) {
  return (
    <>
      <label className="setting-row toggle-row">
        <span>{label}</span>
        <input
          type="checkbox"
          checked={checked}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked)}
        />
      </label>
      {hint && <p className="hint">{hint}</p>}
    </>
  )
}
