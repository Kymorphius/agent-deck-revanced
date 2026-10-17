// matrix/hardened: strict baseline and atomic whole-row delta validation keep
// malformed or skipped publications from corrupting the dashboard state.
import { describe, expect, it } from 'vitest'
import { applyMenuDelta, readMenuBaseline } from '../../../internal/web/static/app/menuDelta.js'

const makeSnapshot = () => ({
  profile: 'work', generatedAt: 'snapshot-one', totalGroups: 1, totalSessions: 2,
  items: [
    { index: 0, type: 'group', level: 0, path: 'work', group: {
      name: 'Work', path: 'work', expanded: true, order: 0, sessionCount: 2, defaultPath: '/old',
    } },
    { index: 1, type: 'session', level: 1, path: 'work', isLastInGroup: true, session: {
      id: 'one', title: 'One', status: 'running', annotation: 'old annotation',
      substateDetail: 'old detail', modelId: 'old model', handoff: { target: 'old' },
    } },
    { index: 2, type: 'session', level: 1, path: 'work', session: { id: 'two', title: 'Two', status: 'idle' } },
  ],
})
const makeFull = () => ({ type: 'menu', version: 'fingerprint-one', data: makeSnapshot() })
const makeDelta = () => ({
  type: 'menu-delta', baseVersion: 'fingerprint-one', version: 'fingerprint-two',
  data: {
    profile: 'work', generatedAt: 'snapshot-two', totalGroups: 1, totalSessions: 2,
    changes: [{ index: 1, item: {
      index: 1, type: 'session', level: 1, path: 'work', session: { id: 'one', title: 'One', status: 'idle' },
    } }],
  },
})
const deepFreeze = value => {
  if (value !== null && typeof value === 'object') {
    for (const child of Object.values(value)) deepFreeze(child)
    Object.freeze(value)
  }
  return value
}

// matrix/hardened: share exact bad rows across full-frame and delta tests;
// NEL/ideographic whitespace follow Go TrimSpace, while NUL is never an identity.
const invalidIdentityValues = [
  ['ordinary whitespace identity', ' \t\r\n'],
  ['NEL whitespace identity', '\u0085'],
  ['ideographic whitespace identity', '\u3000'],
  ['mixed Unicode whitespace identity', ' \u0085\u3000\t'],
  ['NUL-only identity', '\u0000'],
  ['embedded NUL identity', 'valid\u0000identity'],
]
const nonNullOtherPayloads = [
  ['object', { id: 'other', path: 'other' }],
  ['empty object', {}],
  ['array', []],
  ['string', ''],
  ['number', 0],
  ['boolean', false],
]
const invalidItemCases = [
  ...invalidIdentityValues.flatMap(([name, identity]) => [
    [`group ${name}`, 0, item => { item.group.path = identity }],
    [`session ${name}`, 1, item => { item.session.id = identity }],
  ]),
  ...nonNullOtherPayloads.flatMap(([name, payload]) => [
    [`group with non-null session ${name}`, 0, item => { item.session = payload }],
    [`session with non-null group ${name}`, 1, item => { item.group = payload }],
  ]),
]
const validIdentityCases = [
  ['surrounding whitespace', '\t\u0085work\u3000 ', ' \u0085one\u3000\r\n'],
  ['BOM-only', '\uFEFF', '\uFEFF'],
]

describe('menu delta baseline', () => {
  it('establishes an exact versioned full baseline without mutating the frame', () => {
    const message = deepFreeze(makeFull())
    const baseline = readMenuBaseline(message)
    expect(baseline).toEqual({ version: message.version, snapshot: message.data })
    expect(baseline.snapshot).toBe(message.data)
  })

  it('accepts an empty snapshot when both totals are zero', () => {
    const message = makeFull()
    message.data.items = []
    message.data.totalGroups = 0
    message.data.totalSessions = 0
    expect(readMenuBaseline(message)?.snapshot).toBe(message.data)
  })

  it('does not turn legacy full frames into a delta baseline', () => {
    const message = makeFull()
    delete message.version
    expect(readMenuBaseline(message)).toBeNull()
    expect(applyMenuDelta(readMenuBaseline(message), makeDelta())).toBeNull()
  })

  it.each([
    ['wrong frame kind', message => { message.type = 'menu-delta' }],
    ['empty version', message => { message.version = '' }],
    ['numeric version', message => { message.version = 42 }],
    ['null snapshot', message => { message.data = null }],
    ['array snapshot', message => { message.data = [] }],
    ['snapshot error', message => { message.data.error = { code: 'UNAVAILABLE' } }],
    ['missing profile', message => { delete message.data.profile }],
    ['numeric profile', message => { message.data.profile = 1 }],
    ['missing generatedAt', message => { delete message.data.generatedAt }],
    ['empty generatedAt', message => { message.data.generatedAt = '' }],
    ['numeric generatedAt', message => { message.data.generatedAt = 123 }],
    ['missing items', message => { delete message.data.items }],
    ['non-array items', message => { message.data.items = {} }],
    ['negative total', message => { message.data.totalGroups = -1 }],
    ['fractional total', message => { message.data.totalSessions = 1.5 }],
    ['string total', message => { message.data.totalSessions = '2' }],
    ['missing total', message => { delete message.data.totalGroups }],
    ['wrong total length', message => { message.data.totalSessions = 1 }],
    ['wrong per-kind totals', message => { message.data.totalGroups = 2; message.data.totalSessions = 1 }],
    ['null row', message => { message.data.items[1] = null }],
    ['wrong row index', message => { message.data.items[1].index = 2 }],
    ['missing row index', message => { delete message.data.items[1].index }],
    ['string row index', message => { message.data.items[1].index = '1' }],
    ['unknown row kind', message => { message.data.items[1].type = 'other' }],
    ['missing group', message => { delete message.data.items[0].group }],
    ['array group', message => { message.data.items[0].group = [] }],
    ['empty group identity', message => { message.data.items[0].group.path = '' }],
    ['numeric group identity', message => { message.data.items[0].group.path = 1 }],
    ['missing session', message => { delete message.data.items[1].session }],
    ['array session', message => { message.data.items[1].session = [] }],
    ['empty session identity', message => { message.data.items[1].session.id = '' }],
    ['numeric session identity', message => { message.data.items[1].session.id = 1 }],
    ['duplicate session identity', message => { message.data.items[2].session.id = 'one' }],
    ['duplicate group identity', message => {
      message.data.items[2] = { index: 2, type: 'group', group: { path: 'work' } }
      message.data.totalGroups = 2
      message.data.totalSessions = 1
    }],
    ...invalidItemCases.map(([name, index, invalidate]) => [
      name, message => { invalidate(message.data.items[index]) },
    ]),
  ])('rejects %s in the first full frame', (_name, invalidate) => {
    const message = makeFull()
    invalidate(message)
    const before = JSON.stringify(message)
    expect(readMenuBaseline(deepFreeze(message))).toBeNull()
    expect(JSON.stringify(message)).toBe(before)
  })

  it.each([null, undefined, [], 'menu', 1])('rejects malformed frame %j', message => {
    expect(readMenuBaseline(message)).toBeNull()
  })

  it('keeps group and session identity namespaces separate', () => {
    const message = makeFull()
    message.data.items[1].session.id = 'work'
    expect(readMenuBaseline(message)).not.toBeNull()
  })

  // matrix/hardened: JSON omission and null both mean no opposite-kind payload;
  // falsy but non-null values above must not receive that exemption.
  it.each(['absent', 'null'])('accepts %s opposite-kind payloads in a JSON full frame', mode => {
    const message = makeFull()
    if (mode === 'null') {
      message.data.items[0].session = null
      message.data.items[1].group = null
      message.data.items[2].group = null
    }
    const wire = deepFreeze(JSON.parse(JSON.stringify(message)))
    expect(readMenuBaseline(wire)?.snapshot).toBe(wire.data)
  })

  // matrix/hardened: trimming only detects empty identities; it never rewrites
  // their bytes, and BOM is not part of Go/Unicode White_Space.
  it.each(validIdentityCases)('preserves valid %s identities in a full frame', (_name, groupPath, sessionID) => {
    const message = makeFull()
    message.data.items[0].group.path = groupPath
    message.data.items[1].session.id = sessionID
    const wire = deepFreeze(JSON.parse(JSON.stringify(message)))
    const baseline = readMenuBaseline(wire)
    expect(baseline).not.toBeNull()
    expect(baseline.snapshot.items[0].group.path).toBe(groupPath)
    expect(baseline.snapshot.items[1].session.id).toBe(sessionID)
  })

  it('keeps otherwise equal identities distinct when their surrounding whitespace differs', () => {
    const message = makeFull()
    message.data.items[2].session.id = ' one '
    expect(readMenuBaseline(deepFreeze(message))).not.toBeNull()
  })
})

describe('menu delta reducer', () => {
  it('replaces the whole row, removes omitted nested fields and preserves untouched references', () => {
    const baseline = deepFreeze(readMenuBaseline(makeFull()))
    const message = deepFreeze(makeDelta())
    const next = applyMenuDelta(baseline, message)
    expect(next.version).toBe(message.version)
    expect(next.snapshot).not.toBe(baseline.snapshot)
    expect(next.snapshot.items).not.toBe(baseline.snapshot.items)
    expect(next.snapshot.items[0]).toBe(baseline.snapshot.items[0])
    expect(next.snapshot.items[2]).toBe(baseline.snapshot.items[2])
    expect(next.snapshot.items[1]).toBe(message.data.changes[0].item)
    expect(next.snapshot.items[1]).not.toHaveProperty('isLastInGroup')
    for (const field of ['annotation', 'substateDetail', 'modelId', 'handoff']) {
      expect(next.snapshot.items[1].session).not.toHaveProperty(field)
    }
    expect(next.snapshot.generatedAt).toBe('snapshot-two')
    expect(next.snapshot).not.toHaveProperty('changes')
    expect(baseline.snapshot.items[1].session.annotation).toBe('old annotation')
    expect(baseline.snapshot.generatedAt).toBe('snapshot-one')
  })

  it('accepts out-of-order changes, including a whole group row', () => {
    const baseline = readMenuBaseline(makeFull())
    const message = makeDelta()
    message.data.changes.push({ index: 0, item: {
      index: 0, type: 'group', level: 0, path: 'work', group: {
        name: 'Renamed label', path: 'work', expanded: false, order: 0, sessionCount: 2,
      },
    } })
    const next = applyMenuDelta(baseline, message)
    expect(next.snapshot.items[0]).toBe(message.data.changes[1].item)
    expect(next.snapshot.items[0].group).not.toHaveProperty('defaultPath')
    expect(next.snapshot.items[1]).toBe(message.data.changes[0].item)
    expect(next.snapshot.items[2]).toBe(baseline.snapshot.items[2])
  })

  // matrix/hardened: absent/null opposite-kind payloads remain compatible with
  // JSON DTOs for both session and group whole-row replacements.
  it.each(['absent', 'null'])('accepts %s opposite-kind payloads in a JSON delta', mode => {
    const baseline = deepFreeze(readMenuBaseline(makeFull()))
    const message = makeDelta()
    message.data.changes.push({ index: 0, item: makeSnapshot().items[0] })
    if (mode === 'null') {
      message.data.changes[0].item.group = null
      message.data.changes[1].item.session = null
    }
    const wire = deepFreeze(JSON.parse(JSON.stringify(message)))
    const next = applyMenuDelta(baseline, wire)
    expect(next).not.toBeNull()
    expect(next.snapshot.items[1]).toBe(wire.data.changes[0].item)
    expect(next.snapshot.items[0]).toBe(wire.data.changes[1].item)
  })

  it.each(validIdentityCases)('preserves valid %s identities across a delta', (_name, groupPath, sessionID) => {
    const full = makeFull()
    full.data.items[0].group.path = groupPath
    full.data.items[1].session.id = sessionID
    const baseline = deepFreeze(readMenuBaseline(JSON.parse(JSON.stringify(full))))
    const message = makeDelta()
    message.data.changes[0].item.session.id = sessionID
    message.data.changes.push({ index: 0, item: {
      ...makeSnapshot().items[0], group: { ...makeSnapshot().items[0].group, path: groupPath },
    } })
    const wire = deepFreeze(JSON.parse(JSON.stringify(message)))
    const next = applyMenuDelta(baseline, wire)
    expect(next).not.toBeNull()
    expect(next.snapshot.items[0].group.path).toBe(groupPath)
    expect(next.snapshot.items[1].session.id).toBe(sessionID)
  })

  it('requires exact identity bytes even when a replacement differs only in surrounding whitespace', () => {
    const baseline = deepFreeze(readMenuBaseline(makeFull()))
    const message = makeDelta()
    message.data.changes[0].item.session.id = ' one '
    expect(applyMenuDelta(baseline, deepFreeze(message))).toBeNull()
  })

  it('treats generatedAt as opaque metadata and permits an empty changes list', () => {
    const baseline = readMenuBaseline(makeFull())
    const message = makeDelta()
    message.data.generatedAt = 'metadata without a parseable date'
    message.data.changes = []
    const next = applyMenuDelta(baseline, message)
    expect(next.version).toBe(message.version)
    expect(next.snapshot.generatedAt).toBe(message.data.generatedAt)
    expect(next.snapshot.items).toEqual(baseline.snapshot.items)
    expect(next.snapshot.items).not.toBe(baseline.snapshot.items)
    next.snapshot.items.forEach((item, index) => expect(item).toBe(baseline.snapshot.items[index]))
  })

  it('advances the exact fingerprint chain and rejects a skipped or replayed delta', () => {
    const baseline = readMenuBaseline(makeFull())
    const first = makeDelta()
    const next = applyMenuDelta(baseline, first)
    expect(applyMenuDelta(next, first)).toBeNull()
    const second = makeDelta()
    second.baseVersion = first.version
    second.version = 'fingerprint-three'
    second.data.changes[0].item.session.status = 'running'
    expect(applyMenuDelta(baseline, second)).toBeNull()
    expect(applyMenuDelta(next, second)?.version).toBe('fingerprint-three')
  })

  it('never coerces fingerprints that exceed integer precision', () => {
    const full = makeFull()
    full.version = '900719925474099312345678901234567890'
    const baseline = readMenuBaseline(JSON.parse(JSON.stringify(full)))
    const message = makeDelta()
    message.baseVersion = full.version
    message.version = '900719925474099312345678901234567891'
    const next = applyMenuDelta(baseline, JSON.parse(JSON.stringify(message)))
    expect(next.version).toBe(message.version)
    const gap = makeDelta()
    gap.baseVersion = '900719925474099312345678901234567889'
    expect(applyMenuDelta(baseline, gap)).toBeNull()
    expect(baseline.version).toBe(full.version)
  })

  it.each([
    ['wrong frame kind', message => { message.type = 'menu' }],
    ['missing base version', message => { delete message.baseVersion }],
    ['wrong base version', message => { message.baseVersion = 'missing-publication' }],
    ['numeric base version', message => { message.baseVersion = 1 }],
    ['missing new version', message => { delete message.version }],
    ['empty new version', message => { message.version = '' }],
    ['numeric new version', message => { message.version = 1 }],
    ['unchanged new version', message => { message.version = message.baseVersion }],
    ['null data', message => { message.data = null }],
    ['array data', message => { message.data = [] }],
    ['data error', message => { message.data.error = { code: 'UNAVAILABLE' } }],
    ['missing profile', message => { delete message.data.profile }],
    ['different profile', message => { message.data.profile = 'personal' }],
    ['numeric profile', message => { message.data.profile = 1 }],
    ['missing generatedAt', message => { delete message.data.generatedAt }],
    ['empty generatedAt', message => { message.data.generatedAt = '' }],
    ['numeric generatedAt', message => { message.data.generatedAt = 123 }],
    ['negative total', message => { message.data.totalGroups = -1 }],
    ['fractional total', message => { message.data.totalSessions = 1.5 }],
    ['string total', message => { message.data.totalSessions = '2' }],
    ['missing total', message => { delete message.data.totalGroups }],
    ['different group total', message => { message.data.totalGroups = 2 }],
    ['different session total', message => { message.data.totalSessions = 1 }],
    ['swapped per-kind totals', message => { message.data.totalGroups = 2; message.data.totalSessions = 1 }],
    ['missing changes', message => { delete message.data.changes }],
    ['non-array changes', message => { message.data.changes = {} }],
    ['null change', message => { message.data.changes.push(null) }],
    ['array change', message => { message.data.changes.push([]) }],
    ['negative change index', message => { message.data.changes[0].index = -1 }],
    ['fractional change index', message => { message.data.changes[0].index = 1.5 }],
    ['string change index', message => { message.data.changes[0].index = '1' }],
    ['missing change index', message => { delete message.data.changes[0].index }],
    ['out-of-range change index', message => { message.data.changes[0].index = 3 }],
    ['duplicate change index', message => { message.data.changes.push(message.data.changes[0]) }],
    ['missing replacement', message => { delete message.data.changes[0].item }],
    ['array replacement', message => { message.data.changes[0].item = [] }],
    ['mismatched replacement index', message => { message.data.changes[0].item.index = 2 }],
    ['missing replacement index', message => { delete message.data.changes[0].item.index }],
    ['unknown replacement kind', message => { message.data.changes[0].item.type = 'other' }],
    ['changed session identity', message => { message.data.changes[0].item.session.id = 'two' }],
    ['missing session identity', message => { delete message.data.changes[0].item.session.id }],
    ['empty session identity', message => { message.data.changes[0].item.session.id = '' }],
    ['changed row kind', message => {
      message.data.changes[0].item = { index: 1, type: 'group', group: { path: 'one' } }
    }],
    ['changed group identity', message => {
      message.data.changes.push({ index: 0, item: { index: 0, type: 'group', group: { path: 'other' } } })
    }],
    ...invalidItemCases.map(([name, index, invalidate]) => [
      name, message => {
        if (index === 1) invalidate(message.data.changes[0].item)
        else {
          const item = makeSnapshot().items[0]
          invalidate(item)
          message.data.changes.push({ index, item })
        }
      },
    ]),
  ])('atomically rejects %s without changing either input', (_name, invalidate) => {
    const baseline = deepFreeze(readMenuBaseline(makeFull()))
    const message = makeDelta()
    invalidate(message)
    const before = JSON.stringify(message)
    expect(applyMenuDelta(baseline, deepFreeze(message))).toBeNull()
    expect(JSON.stringify(message)).toBe(before)
    expect(baseline).toEqual(readMenuBaseline(makeFull()))
    expect(baseline.snapshot.items[1].session.status).toBe('running')
  })

  // matrix/hardened: matching bad identities in a directly supplied baseline
  // and replacement cannot pass merely because their identity strings agree.
  it.each(invalidItemCases)('rejects a direct corrupt baseline with %s and an identical replacement', (_name, index, invalidate) => {
    const baseline = { version: 'fingerprint-one', snapshot: makeSnapshot() }
    invalidate(baseline.snapshot.items[index])
    const message = makeDelta()
    const matchingChange = { index, item: baseline.snapshot.items[index] }
    if (index === 1) message.data.changes[0] = matchingChange
    else message.data.changes.push(matchingChange)
    const before = JSON.stringify({ baseline, message })
    expect(applyMenuDelta(deepFreeze(baseline), deepFreeze(message))).toBeNull()
    expect(JSON.stringify({ baseline, message })).toBe(before)
  })

  it.each([null, undefined, [], 'menu-delta', 1])('rejects malformed delta frame %j', message => {
    expect(applyMenuDelta(readMenuBaseline(makeFull()), message)).toBeNull()
  })

  it.each([
    ['absent baseline', () => null],
    ['missing version', () => ({ snapshot: makeSnapshot() })],
    ['numeric version', () => ({ version: 1, snapshot: makeSnapshot() })],
    ['missing snapshot', () => ({ version: 'fingerprint-one' })],
    ['count mismatch', () => {
      const baseline = readMenuBaseline(makeFull())
      baseline.snapshot.totalSessions = 1
      return baseline
    }],
    ['duplicate identity', () => {
      const baseline = readMenuBaseline(makeFull())
      baseline.snapshot.items[2].session.id = 'one'
      return baseline
    }],
  ])('rejects a delta against %s', (_name, makeBaseline) => {
    const baseline = makeBaseline()
    expect(applyMenuDelta(deepFreeze(baseline), deepFreeze(makeDelta()))).toBeNull()
  })
})
