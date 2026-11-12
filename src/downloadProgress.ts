import type { DownloadItem } from './rpc/schema.gen'

/** Transient UI state stays pending until the backend reports the download's terminal state. */
export type Download = DownloadItem & { cancelling?: boolean }

/** Progress of the transfer only; extraction and placement don't have a measured percentage. */
export function percent(d: { receivedBytes: number; totalBytes: number }): number {
  if (d.totalBytes <= 0) return 0
  return Math.max(0, Math.min(100, Math.floor((d.receivedBytes / d.totalBytes) * 100)))
}

export function isTransferring(d: Download): boolean {
  return (
    d.state === 'downloading' &&
    !d.cancelling &&
    !d.converting &&
    !d.waitingForGame &&
    (!d.phase || d.phase === 'download')
  )
}

/** Shared by the queue, game page and tiles, so finished transfers aren't called installed. */
export function downloadLabel(d: Download): string {
  if (d.cancelling) return 'Cancelling…'
  if (d.state === 'failed') return 'Failed'
  if (d.state === 'queued') return 'Queued'
  if (d.state === 'installed') return 'Installed'
  if (d.converting) return 'Converting on RomM…'
  if (d.waitingForGame) return 'Waiting for game to close'
  if (d.phase === 'extract') return 'Extracting archives…'
  if (d.phase === 'install') return 'Installing files…'
  return isTransferring(d) ? `Downloading ${percent(d)}%` : 'Preparing…'
}

function hasKnownEmptyTransfer(d: Download): boolean {
  // Go omits fileCount when zero. A processing phase has already resolved the transfer plan.
  return d.totalBytes === 0 && !d.fileCount && (d.phase === 'extract' || d.phase === 'install')
}

export function downloadFileCount(d: Download): string | null {
  if (hasKnownEmptyTransfer(d)) return '0 files'
  if (!d.fileCount || d.fileCount <= 0) return null
  return `${d.fileCount} ${d.fileCount === 1 ? 'file' : 'files'}`
}

/** Placement commits the files atomically; wait for that short step to finish. */
export function canCancel(d: Download): boolean {
  return !d.cancelling && (d.state === 'failed' || d.phase !== 'install')
}

/** Describes source transfer size, keeping remove-only updates distinct from an unresolved plan. */
export function downloadTransferText(d: Download, formatSize: (bytes: number) => string): string {
  const files = downloadFileCount(d)
  const size = hasKnownEmptyTransfer(d)
    ? 'No download required'
    : d.totalBytes > 0
      ? isTransferring(d)
        ? `${formatSize(d.receivedBytes)} of ${formatSize(d.totalBytes)}`
        : `${formatSize(d.totalBytes)} download`
      : 'Calculating download size…'
  return [files, size].filter(Boolean).join(' · ')
}
