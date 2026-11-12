import assert from 'node:assert/strict'
import test from 'node:test'

import {
  canCancel,
  downloadFileCount,
  downloadLabel,
  downloadTransferText,
  isTransferring,
  percent,
} from '../src/downloadProgress.ts'

const transferred = {
  id: 1,
  name: 'Mario Kart 8 Deluxe',
  platformId: 1,
  platformName: 'Switch',
  state: 'downloading',
  receivedBytes: 100,
  totalBytes: 100,
  fileCount: 3,
}

test('a finished transfer stays extracting or installing until the backend confirms installed', () => {
  for (const [phase, label] of [
    ['extract', 'Extracting archives…'],
    ['install', 'Installing files…'],
  ]) {
    const download = { ...transferred, phase }
    assert.equal(downloadLabel(download), label)
    assert.equal(isTransferring(download), false)
  }
  assert.equal(downloadLabel({ ...transferred, state: 'installed' }), 'Installed')
})

test('conversion and emulator waits take precedence over processing phases', () => {
  assert.equal(
    downloadLabel({ ...transferred, phase: 'extract', converting: true }),
    'Converting on RomM…',
  )
  assert.equal(
    downloadLabel({ ...transferred, phase: 'extract', waitingForGame: true }),
    'Waiting for game to close',
  )
  assert.equal(isTransferring({ ...transferred, waitingForGame: true }), false)
})

test('cancellation stays pending until a terminal update and placement cannot be cancelled', () => {
  const pending = { ...transferred, phase: 'extract', cancelling: true }
  assert.equal(downloadLabel(pending), 'Cancelling…')
  assert.equal(isTransferring(pending), false)
  assert.equal(canCancel(pending), false)
  assert.equal(canCancel({ ...transferred, phase: 'install' }), false)
  assert.equal(canCancel({ ...transferred, phase: 'extract' }), true)
})

test('failed processing reports the failure rather than a successful 100% transfer', () => {
  assert.equal(downloadLabel({ ...transferred, state: 'failed', phase: 'install' }), 'Failed')
  assert.equal(isTransferring({ ...transferred, state: 'failed' }), false)
  assert.equal(canCancel({ ...transferred, state: 'failed', phase: 'install' }), true)
})

test('counts source files, with unknown sizes and progress kept honest', () => {
  assert.equal(downloadFileCount(transferred), '3 files')
  assert.equal(downloadFileCount({ ...transferred, fileCount: 1 }), '1 file')
  assert.equal(downloadFileCount({ ...transferred, fileCount: 0 }), null)
  assert.equal(percent({ receivedBytes: 100, totalBytes: 0 }), 0)
  assert.equal(percent({ receivedBytes: -10, totalBytes: 100 }), 0)
  assert.equal(percent({ receivedBytes: 101, totalBytes: 100 }), 100)
  assert.equal(downloadLabel({ ...transferred, phase: 'future' }), 'Preparing…')
})

test('remove-only updates display a known zero transfer while processing', () => {
  for (const phase of ['extract', 'install']) {
    for (const fileCount of [0, undefined]) {
      const empty = { ...transferred, phase, fileCount, receivedBytes: 0, totalBytes: 0 }
      assert.equal(
        downloadTransferText(empty, (n) => `${n}B`),
        '0 files · No download required',
      )
    }
  }
  for (const phase of [undefined, 'download']) {
    const unresolved = { ...transferred, phase, fileCount: undefined, totalBytes: 0 }
    assert.equal(
      downloadTransferText(unresolved, (n) => `${n}B`),
      'Calculating download size…',
    )
  }
})
