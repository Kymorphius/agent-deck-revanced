// matrix/hardened: menu deltas are pure, version-fenced whole-row replacements;
// the transport owns recovery and never needs to send application input.
const isRecord = value => value !== null && typeof value === 'object' && !Array.isArray(value)
const isNonEmptyString = value => typeof value === 'string' && value.length > 0
const isCount = value => Number.isSafeInteger(value) && value >= 0
// matrix/hardened: match Go's TrimSpace identity check without normalizing its
// bytes; Unicode White_Space includes NEL and excludes a non-whitespace BOM.
const isRowIdentity = value => isNonEmptyString(value) && !/^\p{White_Space}+$/u.test(value) && !value.includes('\0')

// matrix/hardened: row identity is tied to its kind, so a delta cannot rename,
// reorder or replace a group with a session while retaining the same index.
function itemIdentity(item, index) {
  if (!isRecord(item) || item.index !== index) return null
  if (item.type === 'group' && item.session == null && isRecord(item.group) && isRowIdentity(item.group.path)) {
    return `group:${item.group.path}`
  }
  if (item.type === 'session' && item.group == null && isRecord(item.session) && isRowIdentity(item.session.id)) {
    return `session:${item.session.id}`
  }
  return null
}

function validMetadata(data) {
  return isRecord(data) && !data.error && typeof data.profile === 'string'
    && isNonEmptyString(data.generatedAt) && isCount(data.totalGroups) && isCount(data.totalSessions)
}

// matrix/hardened: a full frame establishes a delta baseline only when every
// row has a unique identity and the declared totals describe the actual rows.
function validSnapshot(snapshot) {
  if (!validMetadata(snapshot) || !Array.isArray(snapshot.items)
    || snapshot.totalGroups + snapshot.totalSessions !== snapshot.items.length) return false
  const identities = new Set()
  let groups = 0
  let sessions = 0
  for (let index = 0; index < snapshot.items.length; index++) {
    const item = snapshot.items[index]
    const identity = itemIdentity(item, index)
    if (identity === null || identities.has(identity)) return false
    identities.add(identity)
    if (item.type === 'group') groups++
    else sessions++
  }
  return groups === snapshot.totalGroups && sessions === snapshot.totalSessions
}

export function readMenuBaseline(message) {
  // matrix/hardened: fingerprints remain opaque strings, including values
  // beyond JavaScript's integer precision; legacy full frames are not baselines.
  if (!isRecord(message) || message.type !== 'menu' || !isNonEmptyString(message.version)
    || !validSnapshot(message.data)) return null
  return { version: message.version, snapshot: message.data }
}

export function applyMenuDelta(baseline, message) {
  // matrix/hardened: reject missing/corrupt baselines and version gaps before
  // inspecting changes. generatedAt is metadata, never a fingerprint input.
  if (!isRecord(baseline) || !isNonEmptyString(baseline.version) || !validSnapshot(baseline.snapshot)
    || !isRecord(message) || message.type !== 'menu-delta'
    || !isNonEmptyString(message.baseVersion) || message.baseVersion !== baseline.version
    || !isNonEmptyString(message.version) || message.version === message.baseVersion) return null
  const snapshot = baseline.snapshot
  const data = message.data
  if (!validMetadata(data) || data.profile !== snapshot.profile
    || data.totalGroups !== snapshot.totalGroups || data.totalSessions !== snapshot.totalSessions
    || data.totalGroups + data.totalSessions !== snapshot.items.length || !Array.isArray(data.changes)) return null

  // matrix/hardened: validate the entire delta before copying or replacing any
  // row, so one bad late change cannot partially update the displayed state.
  const changedIndexes = new Set()
  for (const change of data.changes) {
    if (!isRecord(change) || !Number.isSafeInteger(change.index) || change.index < 0
      || change.index >= snapshot.items.length || changedIndexes.has(change.index)) return null
    const identity = itemIdentity(change.item, change.index)
    if (identity === null || identity !== itemIdentity(snapshot.items[change.index], change.index)) return null
    changedIndexes.add(change.index)
  }

  const items = snapshot.items.slice()
  for (const change of data.changes) items[change.index] = change.item
  // matrix/hardened: whole-row replacement clears omitted nested fields and
  // preserves the object identity of each unchanged row for UI consumers.
  return {
    version: message.version,
    snapshot: {
      ...snapshot,
      profile: data.profile,
      generatedAt: data.generatedAt,
      totalGroups: data.totalGroups,
      totalSessions: data.totalSessions,
      items,
    },
  }
}
