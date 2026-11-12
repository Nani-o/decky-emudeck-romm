import { addEventListener, removeEventListener, toaster } from '@decky/api'
import { useSyncExternalStore } from 'react'

import { BACKEND_STATUS_EVENT, type BackendStatus, call, onEvent } from './backend'
import { canCancel, type Download } from './downloadProgress'

export { percent } from './downloadProgress'
import type {
  DeleteEvent,
  DownloadEvent,
  DownloadHoldEvent,
  OwnershipEvent,
  Error as RpcError,
} from './rpc/schema.gen'

export interface Downloads {
  /** The queued, running and failed downloads, in queue order. */
  items: Download[]
  /** Why downloading is off, null when it works. */
  problem: RpcError | null
  /**
   * Why the queue is on hold, null when it isn't: the drive filled, and nothing starts until
   * there's room.
   */
  held: RpcError | null
  /**
   * Goes up each time a download ends or fails, a delete changes, or a game is adopted or linked,
   * so views listing states reload.
   */
  ended: number
}

let current: Downloads = { items: [], problem: null, held: null, ended: 0 }
const listeners = new Set<() => void>()

function publish(next: Downloads) {
  current = next
  for (const fn of listeners) fn()
}

async function load(clearCancellation = false) {
  try {
    const r = await call('list_downloads')
    const items = r.downloads.map((d) => ({
      ...d,
      cancelling:
        !clearCancellation &&
        d.state !== 'failed' &&
        current.items.some((was) => was.id === d.id && was.cancelling),
    }))
    publish({ ...current, items, problem: r.problem ?? null, held: r.held ?? null })
  } catch (_e) {
    // The backend is down or restarting; the backend status section says so.
  }
}

function apply(e: DownloadEvent) {
  const was = current.items.find((d) => d.id === e.id)
  if (e.state === 'queued' || e.state === 'downloading' || e.state === 'failed') {
    const next: Download = { ...e, cancelling: e.state !== 'failed' && was?.cancelling }
    const items = was
      ? current.items.map((d) => (d.id === e.id ? next : d))
      : [...current.items, next]
    // Failing moves the game between filters, like ending does.
    const failed = e.state === 'failed' && was?.state !== 'failed'
    publish({ ...current, items, ended: failed ? current.ended + 1 : current.ended })
    // Only a download seen running fails here; one restored as failed when the backend started
    // was reported when it failed.
    if (failed && was && e.problem) {
      toaster.toast({ title: `Couldn't download ${e.name}`, body: e.problem.message })
    }
    return
  }
  publish({
    ...current,
    items: current.items.filter((d) => d.id !== e.id),
    ended: current.ended + 1,
  })
  // A download can end long after the QAM closed.
  if (e.state === 'installed') toaster.toast({ title: 'Installed', body: e.name })
}

function applyDelete(e: DeleteEvent) {
  // Every change moves the game between Installed and On RomM, so lists reload either way.
  publish({ ...current, ended: current.ended + 1 })
  if (e.problem) toaster.toast({ title: `Didn't delete ${e.name}`, body: e.problem.message })
}

function applyOwnership(e: OwnershipEvent) {
  // Each moves the game between Installed and another state, or changes its page.
  publish({ ...current, ended: current.ended + 1 })
  switch (e.kind) {
    case 'adopted':
      toaster.toast({ title: 'Adopted', body: e.name })
      break
    case 'linked':
      toaster.toast({ title: 'Linked to RomM', body: e.name })
      break
    case 'unlinked':
      toaster.toast({ title: 'Unlinked from RomM', body: e.name })
      break
    case 'released':
      toaster.toast({ title: 'Stopped managing', body: e.name })
      break
    case 'update':
      if (e.state === 'update_available') toaster.toast({ title: 'Update available', body: e.name })
      break
    case 'changed':
      // The plugin stopped managing a game on its own, so the user hears why.
      toaster.toast({
        title: `${e.name} changed on disk`,
        body: 'The plugin no longer manages it, and won’t delete it.',
      })
      break
    // A game the user deleted by hand needs no word: it's simply On RomM again.
  }
}

function applyHold(e: DownloadHoldEvent) {
  publish({ ...current, held: e.held ?? null })
  if (e.held) toaster.toast({ title: 'Downloads on hold', body: e.held.message })
}

/**
 * Keeps the download queue current from the backend's download events, and reloads it each time
 * the backend becomes ready. Returns a function that stops listening.
 */
export function watchDownloads(): () => void {
  void load()
  const stop = onEvent('download', apply)
  const stopDeletes = onEvent('delete', applyDelete)
  const stopHolds = onEvent('download_hold', applyHold)
  const stopOwnership = onEvent('ownership', applyOwnership)
  const listener = addEventListener<[BackendStatus]>(BACKEND_STATUS_EVENT, (backend) => {
    if (backend.state === 'ready') void load(true)
  })
  return () => {
    stop()
    stopDeletes()
    stopHolds()
    stopOwnership()
    removeEventListener(BACKEND_STATUS_EVENT, listener)
  }
}

function subscribe(fn: () => void) {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

export function useDownloads(): Downloads {
  return useSyncExternalStore(subscribe, () => current)
}

/** Request cancellation once; it is complete only when its terminal event removes the item. */
export async function cancelDownload(id: number): Promise<void> {
  const item = current.items.find((d) => d.id === id)
  if (item && !canCancel(item)) return
  publish({
    ...current,
    items: current.items.map((d) => (d.id === id ? { ...d, cancelling: true } : d)),
  })
  try {
    const result = await call('cancel_download', { id })
    if (!result.cancelled) {
      publish({
        ...current,
        items: current.items.map((d) => (d.id === id ? { ...d, cancelling: false } : d)),
      })
      await load()
    }
  } catch (err) {
    publish({
      ...current,
      items: current.items.map((d) => (d.id === id ? { ...d, cancelling: false } : d)),
    })
    throw err
  }
}
