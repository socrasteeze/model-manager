import { formatBytes, previewURL, relativeTimeOrEmpty, type SearchHit } from '../api'

/**
 * The badge's tooltip.
 *
 * Says which version you have and which one is newer, because a badge that can
 * only say "update" gives you nothing to decide with -- and says when the
 * answer is from, because a stored result presented without its age reads as
 * freshly checked whether it is a minute or a month old.
 */
function updateTitle(hit: SearchHit): string {
  const have = hit.have_version_name || 'the version you have'
  const latest = hit.latest_version_name || 'a newer version'
  const age = relativeTimeOrEmpty(hit.update_checked_at)
  const when = age ? ` (checked ${age})` : ''
  if (hit.update_base_model_changed) {
    return `${latest} is newer than ${have}, but targets a different base model — not a drop-in replacement${when}`
  }
  return `${have} → ${latest}${when}`
}

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
          {/* In the meta row rather than overlaid on the image: an update is
              about the record, not the picture, and the corners are already
              taken by "missing" and "nsfw". */}
          {hit.update_available && (
            <span className="chip update-chip" title={updateTitle(hit)}>
              {hit.update_base_model_changed ? 'new base' : 'update'}
            </span>
          )}
        </div>
        <div className="card-foot">
          <span>{formatBytes(hit.size)}</span>
          {/* Says the card stands for more than it shows, so a collapsed group
              is visible rather than silently hiding the other versions. */}
          {(hit.group_size ?? 1) > 1 && (
            <span title={`${hit.group_size} versions of this model are in your library`}>
              {hit.group_size} versions
            </span>
          )}
          {hit.path_count > 1 && <span title="copies on disk">×{hit.path_count}</span>}
        </div>
      </div>
    </button>
  )
}
