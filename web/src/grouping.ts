import type { GroupingMode, Listing } from './api'

/**
 * Version grouping for browse results.
 *
 * One Civitai model is published as many versions, and the provider's search
 * returns one result per version -- so searching "realistic vision" returns
 * eight cards with the same name, differing only in a version label buried in
 * the meta row. Collapsing them restores the unit the provider's own page uses.
 *
 * Done here, on results already fetched, rather than server-side: a browse page
 * IS the provider's page, so every version of a grouped model is already in the
 * payload. That makes the version switcher free, and it keeps the API honest --
 * the server still reports what the provider returned.
 *
 * Not done for the library, where a page is 60 arbitrary rows out of thousands
 * and two versions of one model can land on different pages. That collapse has
 * to be a SQL row filter or it renders the same model twice.
 */

export interface ListingGroup {
  /** Stable across renders and unique per rendered card. */
  key: string

  /** Every version in this group, newest first. */
  versions: Listing[]

  /** The version shown by default: the newest, or the one already owned. */
  primary: Listing

  /**
   * What the group as a whole is: 'have' if any version is owned and none is
   * newer, 'outdated' if a newer one exists, 'new' if none is owned.
   */
  status: 'have' | 'outdated' | 'new'

  /** The owned version's label, when one is owned. */
  haveVersionName?: string
}

/** Version ids are numeric strings; a non-numeric one sorts last. */
function numericID(id?: string): number {
  const n = Number(id)
  return Number.isFinite(n) ? n : -1
}

/**
 * Whether a listing is already in the library.
 *
 * Not simply `status === 'have'`. The server's matcher reports 'new' for a
 * version older than the newest one owned -- correct for that single card,
 * since it is not itself owned -- but it still fills in have_version_id to say
 * the *model* is. Reading only the status would make a group whose newest
 * version you own render as entirely new.
 */
function isOwned(l: Listing): boolean {
  return l.local?.status === 'have'
}

function modelIsOwned(l: Listing): boolean {
  return isOwned(l) || !!l.local?.have_version_id
}

/**
 * Groups listings by upstream model.
 *
 * 'architecture' (the default) also requires the same base model, so a LoRA
 * rebuilt from SD 1.5 onto SDXL stays a separate card -- it is published as a
 * new version of the same model but is not a drop-in replacement, and burying
 * it inside the SD 1.5 card's version list would hide exactly that.
 *
 * 'off' returns one group per listing, so every downstream component takes the
 * same shape and nothing needs a branch for the ungrouped case.
 *
 * Input order is preserved between groups: the provider sorted these, and
 * re-sorting would quietly override the sort control.
 */
export function groupListings(items: Listing[], mode: GroupingMode): ListingGroup[] {
  const groups: ListingGroup[] = []
  const byKey = new Map<string, Listing[]>()
  const order: string[] = []

  for (const [i, l] of items.entries()) {
    // The index is what makes 'off' actually mean off. Keying on
    // provider:id:version_id looked equivalent and is not: CivArchive listings
    // carry no version id at all, so several distinct entries for one model
    // collapsed into a single card even with grouping turned off.
    const key =
      mode === 'off'
        ? `${i}:${l.provider}:${l.id}:${l.version_id ?? ''}`
        : mode === 'model'
          ? `${l.provider}:${l.id}`
          : `${l.provider}:${l.id}:${(l.base_model ?? '').toLowerCase()}`

    const existing = byKey.get(key)
    if (existing) {
      existing.push(l)
    } else {
      byKey.set(key, [l])
      order.push(key)
    }
  }

  for (const key of order) {
    const versions = byKey.get(key)!.slice()
    // Newest first, so the default selection and the picker agree.
    versions.sort((a, b) => numericID(b.version_id) - numericID(a.version_id))
    groups.push({ key, versions, ...summarizeStatus(versions) })
  }
  return groups
}

/**
 * Decides what a group as a whole says, and which version it opens on.
 *
 * The primary is the owned version when there is one, not the newest: opening
 * a card on a version you do not have, when you do have another, buries the
 * fact that you own it. The badge still says 'outdated' in that case, so the
 * newer one is one click away.
 */
export function summarizeStatus(versions: Listing[]): {
  primary: Listing
  status: 'have' | 'outdated' | 'new'
  haveVersionName?: string
} {
  const owned = versions.find(isOwned)
  const anyKnown = versions.find(modelIsOwned)
  const newest = versions[0]

  if (!anyKnown) {
    return { primary: newest, status: 'new' }
  }

  const haveVersionName =
    owned?.version_name ?? anyKnown.local?.have_version_name ?? undefined

  // A newer version exists in this group than the one owned.
  const ownedID = owned
    ? numericID(owned.version_id)
    : numericID(anyKnown.local?.have_version_id)
  const outdated = numericID(newest.version_id) > ownedID && ownedID >= 0

  return {
    primary: owned ?? newest,
    status: outdated ? 'outdated' : 'have',
    haveVersionName,
  }
}
