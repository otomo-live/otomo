import { HttpResponse, http } from 'msw'
import { describe, expect, it } from 'vitest'

import {
  getOverview,
  getSeries,
  listLogs,
  listMergedAudit,
  listServices,
  readLogLine,
} from '@/api/dashboard'
import { ApiError } from '@/api/errors'
import { server } from '@/mocks/server'

/**
 * The Dashboard client against the fixture API. Two things it does that are worth
 * checking rather than assuming: it asks for a named scrape status separately from
 * the overview, and it drops an empty filter rather than sending it, because an
 * empty filter is the absence of a filter and the service reads `level=` as "no
 * level given" anyway.
 */

describe('getOverview', () => {
  it('reads the services, the host, the online count and the degraded list', async () => {
    const overview = await getOverview()

    expect(overview.services.map((service) => service.name)).toContain('gateway_dev')
    expect(overview.host.cpuRatio).toBeGreaterThanOrEqual(0)
    expect(overview.host.cpuRatio).toBeLessThanOrEqual(1)
    expect(overview.host.memRatio).toBeGreaterThanOrEqual(0)
    expect(overview.onlinePlayers).toBe(1284)
    expect(overview.degraded).toEqual(['host.disk'])
  })

  it('reads the firing alerts, defaulting an older service to none', async () => {
    const overview = await getOverview()
    expect(overview.alerts).toEqual([
      {
        name: 'GameServerPoolEmpty',
        severity: 'warning',
        service: '',
        summary: 'Allocator game-server pool is empty',
        activeAt: '2026-09-29T02:00:00Z',
      },
    ])
  })

  it('reads a missing or non-array alerts field as an empty list, never an error', async () => {
    server.use(
      http.get('/api/admin/dashboard/overview', () =>
        HttpResponse.json({ generated_at: '2026-09-29T00:00:00Z', services: [] }),
      ),
    )
    await expect(getOverview()).resolves.toMatchObject({ alerts: [] })

    server.use(
      http.get('/api/admin/dashboard/overview', () =>
        HttpResponse.json({ generated_at: '2026-09-29T00:00:00Z', services: [], alerts: 'nope' }),
      ),
    )
    await expect(getOverview()).resolves.toMatchObject({ alerts: [] })
  })

  it('skips alert entries that are not objects and fills missing strings with empty', async () => {
    server.use(
      http.get('/api/admin/dashboard/overview', () =>
        HttpResponse.json({
          generated_at: '2026-09-29T00:00:00Z',
          services: [],
          alerts: ['junk', 12, null, { name: 'OnlyName' }],
        }),
      ),
    )
    const overview = await getOverview()
    expect(overview.alerts).toEqual([
      { name: 'OnlyName', severity: '', service: '', summary: '', activeAt: '' },
    ])
  })

  it('keeps a null figure null rather than coercing it to zero', async () => {
    const overview = await getOverview()
    const gateway = overview.services.find((service) => service.name === 'gateway_dev')

    // The host disk card and one service p95 are `null` in the fixture, which is
    // what a failed query looks like: zero would read as a real measurement.
    expect(overview.host.diskRatio).toBeNull()
    expect(gateway?.p95Ms).toBeNull()
    expect(gateway?.rps).toBeTypeOf('number')
    expect(gateway?.errorRatio).toBeTypeOf('number')
  })

  it('reads up, ready and the not-ready reason as separate facts', async () => {
    const overview = await getOverview()
    const adminAuth = overview.services.find((service) => service.name === 'admin-auth')

    // Up but not ready: the process answered and cannot serve.
    expect(adminAuth?.up).toBe(true)
    expect(adminAuth?.ready).toBe(false)
    expect(adminAuth?.reason).toContain('connection refused')

    const config = overview.services.find((service) => service.name === 'config')
    expect(config?.ready).toBe(true)
    expect(config?.reason).toBe('')
  })

  it('stamps the response, so a fixture with a fixed timestamp cannot read as a live one', async () => {
    const overview = await getOverview()
    expect(Number.isNaN(Date.parse(overview.generatedAt))).toBe(false)
  })

  it('says which services are down, which is the point of the whole card list', async () => {
    const overview = await getOverview()
    expect(
      overview.services.filter((service) => !service.up).map((service) => service.name),
    ).toEqual(['patch'])
  })
})

describe('listServices', () => {
  it('says which scrapes are succeeding, separately from what the last one saw', async () => {
    const services = await listServices()

    expect(services.map((service) => service.name)).toContain('config')
    expect(services.find((service) => service.name === 'patch')).toEqual({
      name: 'patch',
      up: false,
      lastScrapeAt: '2026-09-22T10:09:15Z',
      scrapeError: 'connection refused',
    })
  })

  it('leaves scrapeError empty where the scrape is fine, rather than absent', async () => {
    // A view tests it against `''`, and `undefined` there would print "undefined"
    // into the page, which is why the reader normalises.
    const services = await listServices()
    expect(services.find((service) => service.name === 'config')?.scrapeError).toBe('')
  })
})

describe('listLogs', () => {
  it('reads a line as the four columns it has', async () => {
    const lines = await listLogs({ service: 'config' })

    expect(lines.length).toBeGreaterThan(0)
    for (const line of lines) {
      expect(line.service).toBe('config')
      expect(Number.isNaN(Date.parse(line.at))).toBe(false)
      expect(line.level).not.toBe('')
      expect(line.message).not.toBe('')
    }
  })

  it('filters by level on the server rather than in the browser', async () => {
    const errors = await listLogs({ level: 'error' })
    expect(errors.length).toBeGreaterThan(0)
    for (const line of errors) expect(line.level).toBe('error')
  })

  it('filters by a substring of the message', async () => {
    const all = await listLogs()
    const matched = await listLogs({ contains: 'balance.weapons' })

    expect(matched.length).toBeGreaterThan(0)
    expect(matched.length).toBeLessThan(all.length)
    for (const line of matched) expect(line.message).toContain('balance.weapons')
  })

  it('filters by the request id carried in the structured fields', async () => {
    const matched = await listLogs({ requestId: 'req_0000000000a1' })

    expect(matched).toHaveLength(1)
    expect(matched[0].fields?.request_id).toBe('req_0000000000a1')
    // The param is named `request_id` on the wire, matching the endpoint.
    expect(await listLogs({ requestId: 'no-such-request' })).toEqual([])
  })

  it('parses the fields a line carries, and leaves the rest null', async () => {
    const lines = await listLogs()
    const withFields = lines.filter((line) => line.fields !== null)
    expect(withFields.length).toBeGreaterThan(0)
    expect(typeof withFields[0].fields?.request_id).toBe('string')
    expect(lines.some((line) => line.fields === null)).toBe(true)
  })

  it('caps at the limit it is given', async () => {
    expect(await listLogs({ limit: 2 })).toHaveLength(2)
  })

  it('sends no filter for an empty one, which is the same as not asking', async () => {
    expect(await listLogs({ level: '', service: '' })).toEqual(await listLogs())
  })

  it('is an empty list rather than a refusal when nothing matches', async () => {
    expect(await listLogs({ service: 'no-such-service' })).toEqual([])
  })
})

describe('listMergedAudit', () => {
  it('reads a merged row with the source it came from', async () => {
    const page = await listMergedAudit()

    expect(page.entries.length).toBeGreaterThan(0)
    const sources = new Set(page.entries.map((entry) => entry.source))
    expect([...sources].sort()).toEqual(['admin-auth', 'config'])

    const first = page.entries[0]
    expect(first.id).toBeTypeOf('number')
    expect(first.actorId).not.toBe('')
    expect(first.actorName).not.toBe('')
    expect(first.action).not.toBe('')
    expect(first.target).not.toBe('')
  })

  it('pages with the cursor it was handed, without repeating a row', async () => {
    const first = await listMergedAudit()
    expect(first.nextCursor).toBeTypeOf('string')

    const second = await listMergedAudit({ cursor: first.nextCursor ?? '' })
    expect(second.entries.length).toBeGreaterThan(0)

    const firstIds = new Set(first.entries.map((entry) => entry.id))
    for (const entry of second.entries) expect(firstIds.has(entry.id)).toBe(false)
  })

  it('ends the paging rather than looping when the last page is read', async () => {
    let cursor: string | null = null
    let pages = 0
    do {
      const page = await listMergedAudit(cursor === null ? {} : { cursor })
      cursor = page.nextCursor
      pages += 1
      expect(pages).toBeLessThan(10)
    } while (cursor !== null)

    expect(cursor).toBeNull()
  })

  it('filters by source', async () => {
    const page = await listMergedAudit({ source: 'admin-auth' })
    expect(page.entries.length).toBeGreaterThan(0)
    for (const entry of page.entries) expect(entry.source).toBe('admin-auth')
  })

  it('filters by actor, by id or by the name a reader would recognise', async () => {
    const byName = await listMergedAudit({ actor: 'Liveops' })
    expect(byName.entries.length).toBeGreaterThan(0)
    for (const entry of byName.entries) expect(entry.actorName).toBe('Liveops')

    const byId = await listMergedAudit({ actor: 'usr_liveops' })
    expect(byId.entries.map((entry) => entry.id)).toEqual(byName.entries.map((entry) => entry.id))
  })

  it('is an empty page rather than a refusal when nothing matches', async () => {
    expect(await listMergedAudit({ actor: 'nobody' })).toEqual({
      entries: [],
      nextCursor: null,
      degraded: [],
    })
  })

  it('carries the sources the merge could not read, so the page can say so', async () => {
    const { http, HttpResponse } = await import('msw')
    const { server } = await import('@/mocks/server')
    server.use(
      http.get('*/api/admin/dashboard/audit', () =>
        HttpResponse.json({ entries: [], next_cursor: null, degraded: ['admin-auth'] }),
      ),
    )

    expect((await listMergedAudit()).degraded).toEqual(['admin-auth'])
  })
})

describe('readLogLine', () => {
  it('reads a frame the tail delivered, which arrives untyped', () => {
    expect(
      readLogLine({ at: '2026-09-22T10:15:01Z', service: 'config', level: 'info', message: 'x' }),
    ).toEqual({
      at: '2026-09-22T10:15:01Z',
      service: 'config',
      level: 'info',
      message: 'x',
      fields: null,
    })
  })

  it('reads the structured fields, and keeps them null when there are none', () => {
    const withFields = readLogLine({ message: 'x', fields: { request_id: 'req_1', status: 200 } })
    expect(withFields.fields).toEqual({ request_id: 'req_1', status: 200 })
    // A `fields` that is not an object is not a fields map, and normalises.
    expect(readLogLine({ message: 'x', fields: 'nope' }).fields).toBeNull()
  })

  it('reads a frame that is not an object, rather than throwing inside a stream', () => {
    // The shape a non-JSON frame is wrapped in by the tail, and what a missing
    // field becomes: the reader normalises so a rendering path can never see
    // `undefined`.
    expect(readLogLine(null)).toEqual({ at: '', service: '', level: '', message: '', fields: null })
    expect(readLogLine({ message: 'not json' }).level).toBe('')
  })
})

describe('getSeries', () => {
  const TO = 1_700_000_000
  const FROM = TO - 3600

  it('reads the columnar body with the metadata a chart needs', async () => {
    const result = await getSeries('config', 'rps_by_status', { from: FROM, to: TO, step: 30 })

    expect(result.metric).toBe('rps_by_status')
    expect(result.service).toBe('config')
    expect(result.unit).toBe('req/s')
    expect(result.title).not.toBe('')
    expect(result.t.length).toBeGreaterThan(0)
    // One series per status class, each aligned with t.
    expect(result.series.map((column) => column.name)).toEqual(['2xx', '3xx', '4xx', '5xx'])
    for (const column of result.series) expect(column.values).toHaveLength(result.t.length)
  })

  it('keeps a null gap null rather than coercing it to zero', async () => {
    const result = await getSeries('config', 'latency_p95', { from: FROM, to: TO, step: 30 })
    const values = result.series[0].values

    // The fixture inserts a gap every 97th point to exercise this path.
    expect(values.some((value) => value === null)).toBe(true)
    expect(values.every((value) => value === null || typeof value === 'number')).toBe(true)
  })

  it('reports a window the service clamped to its maximum', async () => {
    const thirtyDays = 30 * 24 * 60 * 60
    const result = await getSeries('config', 'cpu', { from: TO - thirtyDays, to: TO, step: 3600 })

    expect(result.clamped).toBe(true)
    expect(result.to - result.from).toBeLessThanOrEqual(7 * 24 * 60 * 60)
  })

  it('never returns more than the 1500-point cap', async () => {
    const result = await getSeries('config', 'cpu', {
      from: TO - 7 * 24 * 60 * 60,
      to: TO,
      step: 1,
    })
    expect(result.t.length).toBe(1500)
    expect(result.series[0].values).toHaveLength(1500)
  })

  it('refuses an unknown template rather than asking Prometheus', async () => {
    await expect(
      getSeries('config', 'not_a_template', { from: FROM, to: TO, step: 30 }),
    ).rejects.toMatchObject({ code: 'validation_failed' })
  })
})

describe('the dashboard readers surface a COM-5 refusal', () => {
  const refusal = (path: string) =>
    http.get(path, () =>
      HttpResponse.json(
        { error: { code: 'internal', message: 'scrape backend is down', request_id: 'req_x' } },
        { status: 500 },
      ),
    )

  it('maps a 500 from each reader onto the envelope', async () => {
    server.use(
      refusal('/api/admin/dashboard/overview'),
      refusal('/api/admin/dashboard/services'),
      refusal('/api/admin/dashboard/logs'),
      refusal('/api/admin/dashboard/audit'),
    )

    for (const call of [
      () => getOverview(),
      () => listServices(),
      () => listLogs(),
      () => listMergedAudit(),
    ]) {
      const error = await call().catch((caught: unknown) => caught)
      expect(error).toBeInstanceOf(ApiError)
      expect((error as ApiError).status).toBe(500)
      expect((error as ApiError).code).toBe('internal')
    }
  })
})
