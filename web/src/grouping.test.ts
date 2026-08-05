import { groupListings, summarizeStatus } from './grouping'
import type { Listing } from './api'

// A minimal listing. Only the fields grouping actually reads are set, so a
// test failure points at grouping rather than at fixture drift.
function listing(over: Partial<Listing> = {}): Listing {
  return {
    provider: 'civitai',
    id: '42',
    version_id: '100',
    version_name: 'v1.0',
    name: 'A Model',
    base_model: 'SD 1.5',
    ...over,
  } as Listing
}

// A failure count rather than process.exitCode, so this file typechecks
// against the app's browser-only tsconfig without pulling in @types/node for
// one line. The check at the bottom of this file throws, which is what makes
// the runner exit non-zero.
let failures = 0

function run(name: string, fn: () => void) {
  try {
    fn()
    console.log(`ok   ${name}`)
  } catch (e) {
    console.error(`FAIL ${name}: ${(e as Error).message}`)
    failures++
  }
}

function eq(got: unknown, want: unknown, what: string) {
  const g = JSON.stringify(got)
  const w = JSON.stringify(want)
  if (g !== w) throw new Error(`${what}: got ${g}, want ${w}`)
}

run('architecture mode keeps different base models apart', () => {
  const groups = groupListings(
    [
      listing({ version_id: '200', base_model: 'SD 1.5' }),
      listing({ version_id: '100', base_model: 'SD 1.5' }),
      listing({ version_id: '300', base_model: 'SDXL' }),
    ],
    'architecture',
  )
  eq(groups.length, 2, 'group count')
  eq(groups[0].versions.length, 2, 'SD 1.5 versions')
  // Newest first, so the default selection and the picker agree.
  eq(groups[0].versions[0].version_id, '200', 'newest first')
})

run('model mode merges across base models', () => {
  const groups = groupListings(
    [
      listing({ version_id: '200', base_model: 'SD 1.5' }),
      listing({ version_id: '300', base_model: 'SDXL' }),
    ],
    'model',
  )
  eq(groups.length, 1, 'group count')
  eq(groups[0].versions.length, 2, 'versions')
})

run('off mode is one group per listing', () => {
  const groups = groupListings(
    [listing({ version_id: '200' }), listing({ version_id: '100' })],
    'off',
  )
  eq(groups.length, 2, 'group count')
})

// The regression: CivArchive reports no version id at all, so keying on
// provider:id:version_id collapsed distinct entries even with grouping off.
run('off mode separates listings that share a model id and have no version id', () => {
  const groups = groupListings(
    [
      listing({ provider: 'civarchive', version_id: undefined, version_name: undefined }),
      listing({ provider: 'civarchive', version_id: undefined, version_name: undefined }),
      listing({ provider: 'civarchive', version_id: undefined, version_name: undefined }),
    ],
    'off',
  )
  eq(groups.length, 3, 'group count with no version ids')
})

run('a group whose newest version is owned reads as have', () => {
  const s = summarizeStatus([
    listing({ version_id: '200', local: { status: 'have', path: '/models/a.safetensors' } }),
    listing({ version_id: '100' }),
  ])
  eq(s.status, 'have', 'status')
  eq(s.primary.version_id, '200', 'primary')
})

run('a group with a newer version than the one owned reads as outdated', () => {
  const s = summarizeStatus([
    listing({ version_id: '300' }),
    listing({ version_id: '100', local: { status: 'have' } }),
  ])
  eq(s.status, 'outdated', 'status')
  // Opens on the version you own, not the newest: showing a version you do not
  // have, when you have another, buries the fact that you own it.
  eq(s.primary.version_id, '100', 'primary')
})

// The server reports 'new' for a version older than the newest one owned --
// correct for that row, but the group is not new. Reading only the status
// would make a model you own render as entirely unowned.
run('an older version carrying have_version_id does not read as new', () => {
  const s = summarizeStatus([
    listing({ version_id: '100', local: { status: 'new', have_version_id: '200', have_version_name: 'v2.0' } }),
  ])
  eq(s.status, 'have', 'status')
  eq(s.haveVersionName, 'v2.0', 'have version name')
})

run('a group with nothing owned reads as new', () => {
  const s = summarizeStatus([listing({ version_id: '200' }), listing({ version_id: '100' })])
  eq(s.status, 'new', 'status')
  eq(s.primary.version_id, '200', 'primary is the newest')
})

run('input order between groups is preserved', () => {
  const groups = groupListings(
    [listing({ id: 'b' }), listing({ id: 'a' }), listing({ id: 'b', version_id: '99' })],
    'model',
  )
  eq(groups.map((g) => g.versions[0].id), ['b', 'a'], 'group order')
})

if (failures > 0) {
  throw new Error(`${failures} grouping test(s) failed`)
}
console.log('all grouping tests passed')
