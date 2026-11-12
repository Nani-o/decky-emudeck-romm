import assert from 'node:assert/strict'
import test from 'node:test'

import { downloadPlanText, requestDownloadPlan } from '../src/downloadPlan.ts'

const formatSize = (n) => `${n} bytes`
const describeError = (err) => err.message

function deferred() {
  let resolve, reject
  const promise = new Promise((yes, no) => {
    resolve = yes
    reject = no
  })
  return { promise, resolve, reject }
}

async function flush() {
  await Promise.resolve()
}

test('the preview starts loading without using a base-game size, then shows the selected transfer', async () => {
  const request = deferred()
  const published = []
  requestDownloadPlan(
    () => request.promise,
    (preview) => published.push(preview),
    describeError,
  )
  assert.deepEqual(published, [])
  assert.equal(downloadPlanText({ state: 'loading' }, formatSize), 'Calculating download size…')
  request.resolve({ fileCount: 3, totalBytes: 11000 })
  await flush()
  assert.equal(downloadPlanText(published[0], formatSize), '3 files · 11000 bytes download')
})

test('switching games discards the earlier response even if it arrives last', async () => {
  const previous = deferred()
  const next = deferred()
  let current = null
  const stop = requestDownloadPlan(
    () => previous.promise,
    (preview) => {
      current = preview
    },
    describeError,
  )
  stop()
  requestDownloadPlan(
    () => next.promise,
    (preview) => {
      current = preview
    },
    describeError,
  )
  next.resolve({ fileCount: 1, totalBytes: 200 })
  await flush()
  previous.resolve({ fileCount: 3, totalBytes: 11000 })
  await flush()
  assert.equal(downloadPlanText(current, formatSize), '1 file · 200 bytes download')
})

test('leaving the page or changing to a queued state ignores both late success and failure', async () => {
  for (const rejects of [false, true]) {
    const request = deferred()
    const published = []
    const stop = requestDownloadPlan(
      () => request.promise,
      (preview) => published.push(preview),
      describeError,
    )
    stop()
    if (rejects) request.reject(new Error('offline'))
    else request.resolve({ fileCount: 3, totalBytes: 11000 })
    await flush()
    assert.deepEqual(published, [])
  }
})

test('preview failures are unavailable rather than a plausible base-game total', async () => {
  const published = []
  requestDownloadPlan(
    () => Promise.reject(new Error('offline')),
    (preview) => published.push(preview),
    describeError,
  )
  await flush()
  assert.equal(downloadPlanText(published[0], formatSize), 'Download size unavailable: offline')
})

test('remove-only updates have a complete zero plan rather than a pending estimate', async () => {
  const published = []
  requestDownloadPlan(
    () => Promise.resolve({ fileCount: 0, totalBytes: 0 }),
    (preview) => published.push(preview),
    describeError,
  )
  await flush()
  assert.equal(downloadPlanText(published[0], formatSize), '0 files · No download required')
})

test('a malformed preview cannot display a made-up count or size', async () => {
  const published = []
  requestDownloadPlan(
    () => Promise.resolve({ fileCount: 3, totalBytes: -1 }),
    (preview) => published.push(preview),
    describeError,
  )
  await flush()
  assert.equal(published[0].state, 'unavailable')
})
