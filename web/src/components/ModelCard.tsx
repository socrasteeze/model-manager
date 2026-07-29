import { formatBytes, previewURL, type SearchHit } from '../api'

interface Props {
  hit: SearchHit
  selected: boolean
  onSelect: () => void
}

export function ModelCard({ hit, selected, onSelect }: Props) {
  const title = hit.name || hit.filename || hit.sha256.slice(0, 12)

  return (
    <button
      className={`card${selected ? ' selected' : ''}${hit.present ? '' : ' absent'}`}
      onClick={onSelect}
      title={hit.path || title}
    >
      <div className="card-image">
        {hit.preview_image ? (
          <img src={previewURL(hit.preview_image)} alt="" loading="lazy" decoding="async" />
        ) : (
          <div className="placeholder" aria-hidden="true">
            {(hit.type || '?').slice(0, 2).toUpperCase()}
          </div>
        )}
        {!hit.present && <span className="badge absent-badge">missing</span>}
        {hit.nsfw && <span className="badge nsfw-badge">nsfw</span>}
      </div>

      <div className="card-body">
        <div className="card-title">{title}</div>
        <div className="card-meta">
          {hit.type && <span className="chip">{hit.type}</span>}
          {hit.base_model && <span className="chip subtle">{hit.base_model}</span>}
        </div>
        <div className="card-foot">
          <span>{formatBytes(hit.size)}</span>
          {hit.path_count > 1 && <span title="copies on disk">×{hit.path_count}</span>}
        </div>
      </div>
    </button>
  )
}
