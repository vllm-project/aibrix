/**
 * Module-level store for AI creation data that needs to survive
 * SPA navigation but can't be serialized in React Router state.
 *
 * - pendingCreation: File + blob URL for the current creation (set by
 *   AICreationPage, consumed once by ChatPage)
 * - messageCache: Map of conversation ID → creation messages, so
 *   clicking a sidebar link for a creation conversation re-displays results
 */

import type { ImageData, VideoJobResponse } from '@/api/client'

export interface CreationMessage {
  id: string
  role: 'user' | 'assistant'
  content: string
  model?: string
  mode?: 'image' | 'video'
  ratio?: string
  referenceImageUrl?: string
  images?: ImageData[]
  videoJob?: VideoJobResponse
  loading?: boolean
  error?: string
}

/** Non-serializable data for the current creation (consumed once). */
export const pendingCreation: { file?: File; blobUrl?: string } = {}

/** Cache of creation messages keyed by conversation ID. */
const messageCache = new Map<string, CreationMessage[]>()

export function cacheMessages(conversationId: string, msgs: CreationMessage[]) {
  messageCache.set(conversationId, msgs)
}

export function getCachedMessages(conversationId: string): CreationMessage[] | undefined {
  return messageCache.get(conversationId)
}
