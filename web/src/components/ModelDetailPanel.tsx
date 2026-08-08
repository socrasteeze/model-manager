import { useCallback, useEffect, useRef, useState } from 'react'
import {
  acceptSuggestion,
  archiveComplete,
  config,
  dismissSuggestion,
  enrichModel,
  evictLocal,
  formatBytes,
  getCandidates,
  getModel,
  setTags,
  updateModel,
  type CandidateView,
  type EnrichResult,
  type ModelDetail,
  type PulledCopy,
} from '../api'
import { CopyButton } from './CopyButton'
import { EditableField } from './EditableField'
import { PreviewEditor } from './PreviewEditor'

interface Props {
  sha: string
  onClose: () => void
  onChanged: () => void
}

export function ModelDetailPanel({ sha, onClose, onChanged }: Props) {
  const [detail, setDetail] = useState<ModelDetail | null>(null)
  const [candidates, setCandidates] = useState<CandidateView[] | null>(null)
  const [showProvenance, setShowProvenance] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [refreshNote, setRefreshNote] = useState<string | null>(null)
  const [evicting, setEvicting] = useState(false)
  const [evictError, setEvictError] = useState<string | null>(null)

  const load = useCallback(() => {
    getModel(sha).then(setDetail).catch((e: Error) => setError(e.message))
  }, [sha])

  useEffect(() => {
    setDetail(null)
    setCandidates(null)
    setShowProvenance(false)
    setError(null)
    setRefreshNote(null)
    setEvictError(null)
    load()
  }, [sha, load])

  // Delete one local copy that was pulled from an upstream.
  //
  // Confirmed inline rather than silently, and the confirmation names the exact
  // path and says what survives -- this is the only action in the app that
  // removes a file, and the user should not have to guess how much of their
  // work goes with it. (The answer is none: everything is keyed on the content
  // hash, so the record, the tags, the previews and their own edits all stay.)
  const evict = async (copy: PulledCopy) => {
    const ok = window.confirm(
      `Remove this copy from this machine?\n\n${copy.path}\n\n` +
        `Frees ${formatBytes(copy.size_bytes)}. Everything the library knows about it — ` +
        `name, tags, previews, provenance, your edits — is kept, and it stays listed as ` +
        `available from ${copy.upstream}.`,
    )
    if (!ok) return
    setEvicting(true)
    setEvictError(null)
    try {
      const res = await evictLocal(sha, { path: copy.path, upstream: copy.upstream })
      if (res.detail) setDetail(res.detail)
      else load()
      onChanged()
    } catch (e) {
      setEvictError((e as Error).message)
    } finally {
      setEvicting(false)
    }
  }

  // Ask the origin about this model and merge what comes back.
  //
  // The server decides what wins, by the same rules every other ingest goes
  // through: a value typed here is never overwritten (a contradicting origin
  // becomes a suggestion above instead), a blank field takes the origin's
  // answer, and a chosen thumbnail stays the thumbnail.
  const refresh = async () => {
    setRefreshing(true)
    setRefreshNote(null)
    setError(null)
    try {
      const { detail: fresh, result } = await enrichModel(sha)
      setDetail(fresh)
      // The provenance list is now stale, and it is cached until reopened.
      setCandidates(null)
      setRefreshNote(describeEnrich(result))
      onChanged()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setRefreshing(false)
    }
  }

  useEffect(() => {
    if (!showProvenance || candidates) return
    getCandidates(sha).then(setCandidates).catch(() => setCandidates([]))
  }, [showProvenance, candidates, sha])

  // Escape closes the panel. On a phone this is the back gesture's job, but on a
  // desktop it is the thing everyone reaches for first.
  //
  // Gated on actual visibility: the panel stays mounted while the Browse tab
  // is shown (the library is hidden, not unmounted), and a global listener
  // firing there would silently clear the library's selection from a key
  // pressed on a different screen.
  const rootRef = useRef<HTMLElement>(null)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      const el = rootRef.current
      if (!el || el.closest('[hidden]')) return
      onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const save = async (field: string, value: unknown) => {
    setBusy(true)
    try {
      await updateModel(sha, { [field]: value })
      load()
      onChanged()
      setError(null)
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (error && !detail) {
    return (
      <aside className="detail" ref={rootRef}>
        <button className="close" onClick={onClose} aria-label="Close">×</button>
        <div className="error">{error}</div>
      </aside>
    )
  }

  if (!detail) {
    return (
      <aside className="detail" ref={rootRef}>
        <button className="close" onClick={onClose} aria-label="Close">×</button>
        <div className="loading">Loading…</div>
      </aside>
    )
  }

  const rec = detail.record
  const editable = !config.readOnly
  // Both conditions are enforced server-side too; checking them here as well
  // keeps the button from being a thing you press only to be told no.
  const canRefresh = editable && config.enrichAvailable
  const title = rec?.name || detail.paths[0]?.Path.split(/[/\\]/).pop() || sha.slice(0, 12)

  return (
    <aside className="detail" ref={rootRef}>
      <button className="close" onClick={onClose} aria-label="Close">×</button>

      <PreviewEditor sha={sha} previews={detail.previews} onChanged={() => { void load(); onChanged() }} />

      <h2>{title}</h2>

      {/* Offered only on a writable daemon with remote lookups enabled. */}
      {canRefresh && (
        <div className="refresh-row">
          <button className="refresh-origin" disabled={refreshing} onClick={() => void refresh()}>
            {refreshing ? 'Checking the origin…' : 'Refresh from origin'}
          </button>
          {refreshNote && <span className="source-note">{refreshNote}</span>}
        </div>
      )}

      {error && <div className="error inline">{error}</div>}

      {detail.suggestions.length > 0 && (
        <section className="suggestions">
          <h3>Pending suggestions</h3>
          {/* Manual values are never overwritten by ingest, but a manual value
              that is simply wrong would otherwise be invisible and permanent. */}
          {detail.suggestions.map((s) => (
            <div key={s.id} className="suggestion">
              <div>
                <strong>{s.field}</strong> — {s.source} says{' '}
                <code>{trimJSON(s.suggested_value)}</code>, yours is{' '}
                <code>{trimJSON(s.manual_value)}</code>
              </div>
              {editable && (
                <div className="suggestion-actions">
                  <button
                    disabled={busy}
                    onClick={async () => {
                      setBusy(true)
                      try {
                        await acceptSuggestion(s.id)
                        load()
                        onChanged()
                      } finally {
                        setBusy(false)
                      }
                    }}
                  >
                    Accept
                  </button>
                  <button
                    disabled={busy}
                    onClick={async () => {
                      setBusy(true)
                      try {
                        await dismissSuggestion(s.id)
                        load()
                      } finally {
                        setBusy(false)
                      }
                    }}
                  >
                    Keep mine
                  </button>
                </div>
              )}
            </div>
          ))}
        </section>
      )}

      {rec?.trigger_words && rec.trigger_words.length > 0 && (
        <section className="triggers">
          <h3>Trigger words</h3>
          {/* Use case 5: looking these up on a phone while generating from the
              couch. One tap to copy is the entire interaction. */}
          <div className="trigger-list">
            {rec.trigger_words.map((w) => (
              <CopyButton key={w} value={w} className="trigger" />
            ))}
          </div>
          {rec.trigger_words.length > 1 && (
            <CopyButton value={rec.trigger_words.join(', ')} label="Copy all" className="copy-all" />
          )}
        </section>
      )}

      <section className="fields">
        <EditableField label="Name" value={rec?.name} editable={editable} onSave={(v) => save('name', v)} />
        <EditableField
          label="Type"
          value={rec?.type}
          editable={editable}
          options={['checkpoint', 'lora', 'lycoris', 'vae', 'embedding', 'controlnet', 'upscaler']}
          onSave={(v) => save('type', v)}
        />
        <EditableField
          label="Base model"
          value={rec?.base_model}
          editable={editable}
          onSave={(v) => save('base_model', v)}
        />
        <EditableField label="Version" value={rec?.version} editable={editable} onSave={(v) => save('version', v)} />
        <EditableField
          label="Recommended weight"
          value={rec?.recommended_weight?.toString()}
          editable={editable}
          onSave={(v) => save('recommended_weight', v === null ? null : Number(v))}
        />
        <EditableField
          label="Trigger words"
          value={rec?.trigger_words?.join(', ')}
          editable={editable}
          onSave={(v) => save('trigger_words', v === null ? null : String(v).split(',').map((s) => s.trim()).filter(Boolean))}
        />
        <EditableField
          label="Description"
          value={rec?.description}
          editable={editable}
          multiline
          onSave={(v) => save('description', v)}
        />
        <Row label="Origin" value={rec?.origin} />
      </section>

      <section className="tags-section">
        <h3>Tags</h3>
        <TagEditor
          sha={sha}
          tags={detail.tags}
          editable={editable}
          onSaved={() => {
            load()
            onChanged()
          }}
        />
      </section>

      {detail.training && (
        <section className="training">
          <h3>Training</h3>
          <Row label="Trainer" value={detail.training.trainer} />
          <Row label="Base" value={detail.training.base} />
          <Row label="Dataset" value={detail.training.dataset} />
          <Row
            label="Dataset size"
            value={detail.training.dataset_size ? `${detail.training.dataset_size} images` : undefined}
          />
          <Row label="Run date" value={detail.training.run_date?.slice(0, 10)} />
          {detail.training.notes && <Row label="Notes" value={detail.training.notes} />}
          {detail.training.config && (
            <div className="config-grid">
              {Object.entries(detail.training.config).map(([k, v]) => (
                <div key={k} className="config-item">
                  <span>{k}</span>
                  <code>{String(v)}</code>
                </div>
              ))}
            </div>
          )}
          <p className="source-note">from {detail.training.source.replace(/_/g, ' ')}</p>
        </section>
      )}

      <section className="files">
        <h3>Files</h3>
        {detail.paths.map((p) => {
          // Evicting is offered per-path and only for the copy this daemon
          // actually pulled: a tier-staged copy and an original can share the
          // hash, and the server refuses to guess between them anyway.
          const pulled = (detail.pulled ?? []).find(
            (c) => !c.evicted_at && samePath(c.path, p.Path),
          )
          return (
            <div key={p.ID} className={`path${p.Present ? '' : ' absent'}`}>
              <code>{p.Path}</code>
              <div className="path-badges">
                {!p.Present && <span className="badge absent-badge">not on disk</span>}
                {p.Provisional && (
                  <span className="badge warn-badge" title="Bound by sampled probe; run mm verify --provisional to confirm">
                    provisional
                  </span>
                )}
                {config.evictAvailable && p.Present && !p.Provisional && pulled && (
                  <button
                    className="ghost"
                    disabled={evicting}
                    onClick={() => evict(pulled)}
                    // Never "Delete". This removes a copy that can be fetched
                    // again and keeps everything the library knows, and the
                    // word has to carry that difference.
                    title={`Remove this copy from this machine. It stays listed as available from ${pulled.upstream}.`}
                  >
                    {evicting ? 'Evicting…' : 'Evict local copy'}
                  </button>
                )}
              </div>
            </div>
          )
        })}
        {(detail.pulled ?? []).length > 0 && (
          <p className="source-note">
            {(detail.pulled ?? []).some((c) => !c.evicted_at)
              ? `Pulled from ${detail.pulled![0].upstream}.`
              : `Not on this machine. Available from ${detail.pulled![0].upstream} — pull it again from the Browse tab.`}
          </p>
        )}
        {evictError && <div className="error inline">{evictError}</div>}

        {/* The state the archive exists for. Said plainly, because a model whose
            upstream is gone is one where this copy is the record. */}
        {(detail.archive ?? []).map((a) => (
          <p key={`${a.provider}/${a.model_id}/${a.version_id}`} className="source-note">
            {a.upstream_gone_at ? (
              <strong>
                Archived from {a.provider}, and no longer available there. This is the
                surviving copy.
              </strong>
            ) : (
              <>Archived from {a.provider} (model {a.model_id}, version {a.version_id}).</>
            )}
            {!archiveComplete(a) && (
              <> Incomplete: {a.previews_got} of {a.previews_total} previews stored.</>
            )}
          </p>
        ))}
      </section>

      <section className="identity">
        <h3>Identity</h3>
        <div className="hash-row">
          <span>SHA256</span>
          <CopyButton value={detail.sha256} label={short(detail.sha256)} className="hash" />
        </div>
        {detail.weights_sha256 ? (
          <div className="hash-row">
            <span title="Survives a tool rewriting the header in place">Weights</span>
            <CopyButton value={detail.weights_sha256} label={short(detail.weights_sha256)} className="hash" />
          </div>
        ) : (
          <p className="source-note">
            No weights hash: {detail.format === 'ckpt' || detail.format === 'pt'
              ? 'pickle formats are never parsed, so the tensor region cannot be located'
              : 'the format header did not parse'}
            .
          </p>
        )}
        <Row label="Size" value={formatBytes(detail.size)} />
        <Row label="Format" value={detail.format} />
        <Row label="First seen" value={detail.first_seen?.slice(0, 10)} />
      </section>

      <section className="provenance">
        <button className="link" onClick={() => setShowProvenance((v) => !v)}>
          {showProvenance ? 'Hide' : 'Show'} where each value came from
        </button>
        {showProvenance && candidates && (
          <div className="candidates">
            {candidates.length === 0 && <p className="source-note">No recorded sources.</p>}
            {candidates.map((c) => (
              <div key={c.field} className="candidate">
                <div className="candidate-field">{c.field}</div>
                <div className="candidate-entry winner">
                  <span className={`tier tier-${c.winner.tier_name}`}>{c.winner.tier_name}</span>
                  {/* trimJSON, as the suggestions list already does. A raw
                      stringify of a description field is the whole description
                      on one line, which buried the tier and source this row
                      exists to show. */}
                  <code title={JSON.stringify(c.winner.value)}>{trimJSON(JSON.stringify(c.winner.value))}</code>
                  <span className="src">{c.winner.source}</span>
                </div>
                {c.losers?.map((l, i) => (
                  <div key={i} className="candidate-entry loser">
                    <span className={`tier tier-${l.tier_name}`}>{l.tier_name}</span>
                    <code title={JSON.stringify(l.value)}>{trimJSON(JSON.stringify(l.value))}</code>
                    <span className="src">{l.source}</span>
                  </div>
                ))}
              </div>
            ))}
          </div>
        )}
      </section>
    </aside>
  )
}

function Row({ label, value }: { label: string; value?: string }) {
  if (!value) return null
  return (
    <div className="row">
      <span className="row-label">{label}</span>
      <span className="row-value">{value}</span>
    </div>
  )
}

function TagEditor({
  sha,
  tags,
  editable,
  onSaved,
}: {
  sha: string
  tags: string[]
  editable: boolean
  onSaved: () => void
}) {
  const [draft, setDraft] = useState('')
  const [busy, setBusy] = useState(false)

  const commit = async (next: string[]) => {
    setBusy(true)
    try {
      await setTags(sha, next)
      onSaved()
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="tag-editor">
      <div className="tag-list">
        {tags.length === 0 && <span className="source-note">none</span>}
        {tags.map((t) => (
          <span key={t} className="tag">
            {t}
            {editable && (
              <button
                disabled={busy}
                onClick={() => commit(tags.filter((x) => x !== t))}
                aria-label={`Remove ${t}`}
              >
                ×
              </button>
            )}
          </span>
        ))}
      </div>
      {editable && (
        <form
          onSubmit={(e) => {
            e.preventDefault()
            const value = draft.trim()
            if (!value || tags.includes(value)) return
            setDraft('')
            commit([...tags, value])
          }}
        >
          <input
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            placeholder="Add tag…"
            disabled={busy}
          />
        </form>
      )}
    </div>
  )
}

function short(hash: string): string {
  return `${hash.slice(0, 8)}…${hash.slice(-6)}`
}

// samePath matches a recorded path against a pulled copy's.
//
// Separator- and case-insensitive, because one of the two was written by the
// scanner and the other by the downloader, and on Windows they can differ in
// both. The server does the authoritative comparison; this only decides which
// row gets the button.
function samePath(a: string, b: string): boolean {
  const norm = (s: string) => s.replace(/\\/g, '/').replace(/\/+$/, '').toLowerCase()
  return norm(a) === norm(b)
}

// describeEnrich says what the refresh actually did.
//
// Worth spelling out rather than silently re-rendering: with a manual value in
// place the visible panel can be identical afterwards, and a button that appears
// to do nothing reads as broken. "Not listed" is also a real answer, and the one
// you get for a LoRA you trained yourself.
function describeEnrich(r: EnrichResult): string {
  if (!r.found) {
    return r.errors > 0
      ? 'The origin could not be reached.'
      : 'Not listed at the origin — nothing to merge.'
  }
  const parts: string[] = []
  const added = r.previews_after - r.previews_before
  if (added > 0) parts.push(`${added} new image${added === 1 ? '' : 's'}`)
  parts.push(r.from_archive ? 'merged from the archived response' : 'metadata merged')
  return `${parts.join(', ')}. Values you edited were kept.`
}

function trimJSON(encoded: string): string {
  try {
    const v = JSON.parse(encoded)
    const s = Array.isArray(v) ? v.join(', ') : String(v)
    return cap(s)
  } catch {
    // Capped here too. This branch is the one live path by which an uncapped
    // string reached the page: everything else goes through JSON.parse, so a
    // value that is not valid JSON was the single case that skipped the limit.
    return cap(encoded)
  }
}

function cap(s: string): string {
  return s.length > 60 ? `${s.slice(0, 57)}…` : s
}
