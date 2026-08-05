import type { Facets, Filters } from '../api'

interface Props {
  facets: Facets | null
  filters: Filters
  onChange: (f: Filters) => void
}

export function FilterPanel({ facets, filters, onChange }: Props) {
  const toggle = (key: 'type' | 'base_model' | 'tag', value: string) => {
    const current = filters[key]
    const next = current.includes(value)
      ? current.filter((v) => v !== value)
      : [...current, value]
    onChange({ ...filters, [key]: next })
  }

  const active =
    filters.type.length ||
    filters.base_model.length ||
    filters.tag.length ||
    filters.present !== undefined ||
    filters.needs_attention ||
    filters.needs_update

  return (
    <div className="filters">
      <Section title="Type" counts={facets?.types} selected={filters.type} onToggle={(v) => toggle('type', v)} />
      <Section
        title="Base model"
        counts={facets?.base_models}
        selected={filters.base_model}
        onToggle={(v) => toggle('base_model', v)}
      />
      <Section title="Tags" counts={facets?.tags} selected={filters.tag} onToggle={(v) => toggle('tag', v)} limit={20} />

      <div className="filter-section">
        <h3>Status</h3>
        <label className="check">
          <input
            type="checkbox"
            checked={filters.present === false}
            onChange={(e) => onChange({ ...filters, present: e.target.checked ? false : undefined })}
          />
          {/* An absent path is history, not inventory -- but it is how you find
              out what a tool moved or deleted behind your back. */}
          <span>Missing from disk</span>
        </label>
        <label className="check">
          <input
            type="checkbox"
            checked={!!filters.needs_attention}
            onChange={(e) => onChange({ ...filters, needs_attention: e.target.checked || undefined })}
          />
          <span>Needs attention</span>
        </label>
        {/* Only offered once a check has actually found something. A
            permanently-zero control invites the question "why is this always
            empty", which the answer to is "you have not run a check" -- said
            far better by the button in Settings than by a dead checkbox. */}
        {(facets?.needs_update ?? 0) > 0 && (
          <label className="check">
            <input
              type="checkbox"
              checked={!!filters.needs_update}
              onChange={(e) => onChange({ ...filters, needs_update: e.target.checked || undefined })}
            />
            <span className="check-label">Needs update</span>
            <span className="count">{(facets?.needs_update ?? 0).toLocaleString()}</span>
          </label>
        )}
      </div>

      {/* Last, not first. Appearing above the facet lists pushed Type, Base
          model and Tags down every time a filter went on, which is a jump in
          the thing you are currently reading. At the end it only grows the
          sidebar's scroll extent. Reserving the space instead would waste it
          permanently, and rendering it always-but-disabled leaves a control
          whose only state is "no". */}
      {active ? (
        <button
          className="clear"
          onClick={() => onChange({ ...filters, type: [], base_model: [], tag: [], present: undefined, needs_attention: undefined, needs_update: undefined })}
        >
          Clear filters
        </button>
      ) : null}
    </div>
  )
}

interface SectionProps {
  title: string
  counts?: Record<string, number>
  selected: string[]
  onToggle: (value: string) => void
  limit?: number
}

function Section({ title, counts, selected, onToggle, limit }: SectionProps) {
  if (!counts) return null

  // Most frequent first: in a 19k library the long tail of one-off values is
  // noise, and alphabetical order buries the buckets that actually partition it.
  const entries = Object.entries(counts)
    .filter(([k]) => k !== '')
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))

  if (entries.length === 0) return null
  const shown = limit ? entries.slice(0, limit) : entries

  return (
    <div className="filter-section">
      <h3>{title}</h3>
      {shown.map(([value, count]) => (
        <label key={value} className="check">
          <input type="checkbox" checked={selected.includes(value)} onChange={() => onToggle(value)} />
          <span className="check-label">{value}</span>
          <span className="count">{count.toLocaleString()}</span>
        </label>
      ))}
      {limit && entries.length > limit && (
        <p className="more-note">+{entries.length - limit} more</p>
      )}
    </div>
  )
}
