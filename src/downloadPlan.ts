import type { DownloadPlan } from './rpc/schema.gen'

export type DownloadPlanPreview =
  | { state: 'loading' }
  | { state: 'ready'; plan: DownloadPlan }
  | { state: 'unavailable'; message: string }

/** A preview never changes the queue, and an obsolete request never updates the current page. */
export function requestDownloadPlan(
  fetch: () => Promise<DownloadPlan>,
  publish: (preview: DownloadPlanPreview) => void,
  describeError: (err: unknown) => string,
): () => void {
  let active = true
  void fetch().then(
    (plan) => {
      if (!active) return
      if (
        !Number.isSafeInteger(plan.fileCount) ||
        plan.fileCount < 0 ||
        !Number.isSafeInteger(plan.totalBytes) ||
        plan.totalBytes < 0
      ) {
        publish({ state: 'unavailable', message: 'Invalid download size returned by the server.' })
        return
      }
      publish({ state: 'ready', plan })
    },
    (err: unknown) => {
      if (active) publish({ state: 'unavailable', message: describeError(err) })
    },
  )
  return () => {
    active = false
  }
}

/** Pending and unavailable previews never fall back to the base game's catalog size. */
export function downloadPlanText(
  preview: DownloadPlanPreview,
  formatSize: (bytes: number) => string,
): string {
  if (preview.state === 'loading') return 'Calculating download size…'
  if (preview.state === 'unavailable') return `Download size unavailable: ${preview.message}`
  const { fileCount, totalBytes } = preview.plan
  if (fileCount === 0 && totalBytes === 0) return '0 files · No download required'
  return `${fileCount} ${fileCount === 1 ? 'file' : 'files'} · ${formatSize(totalBytes)} download`
}
