import { useCallback, useRef, useState } from 'react'

interface AudioRecordingState {
  isRecording: boolean
  duration: number
  start: () => Promise<void>
  stop: () => Promise<File>
  cancel: () => void
}

export function useAudioRecording(): AudioRecordingState {
  const [isRecording, setIsRecording] = useState(false)
  const [duration, setDuration] = useState(0)
  const recorderRef = useRef<MediaRecorder | null>(null)
  const chunksRef = useRef<Blob[]>([])
  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const resolveRef = useRef<((file: File) => void) | null>(null)

  const cleanup = useCallback(() => {
    if (timerRef.current) {
      clearInterval(timerRef.current)
      timerRef.current = null
    }
    if (recorderRef.current?.state !== 'inactive') {
      try {
        recorderRef.current?.stop()
      } catch {
        /* already stopped */
      }
    }
    for (const t of recorderRef.current?.stream.getTracks() ?? []) t.stop()
    recorderRef.current = null
    chunksRef.current = []
    resolveRef.current = null
    setIsRecording(false)
    setDuration(0)
  }, [])

  const start = useCallback(async () => {
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true })

    const mimeType = MediaRecorder.isTypeSupported('audio/webm') ? 'audio/webm' : 'audio/mp4'
    const recorder = new MediaRecorder(stream, { mimeType })
    recorderRef.current = recorder
    chunksRef.current = []

    recorder.ondataavailable = (e) => {
      if (e.data.size > 0) chunksRef.current.push(e.data)
    }

    recorder.onstop = () => {
      // Stop mic tracks only AFTER all data has been collected
      for (const t of recorder.stream.getTracks()) t.stop()
      const blob = new Blob(chunksRef.current, { type: mimeType })
      const ext = mimeType === 'audio/webm' ? 'webm' : 'm4a'
      const file = new File([blob], `recording.${ext}`, { type: mimeType })
      resolveRef.current?.(file)
    }

    // Use timeslice to collect data every second during recording,
    // rather than buffering everything until stop().
    recorder.start(1000)
    setIsRecording(true)
    setDuration(0)
    timerRef.current = setInterval(() => {
      setDuration((d) => d + 1)
    }, 1000)
  }, [])

  const stop = useCallback((): Promise<File> => {
    return new Promise((resolve) => {
      resolveRef.current = resolve
      recorderRef.current?.stop()
      if (timerRef.current) {
        clearInterval(timerRef.current)
        timerRef.current = null
      }
      // Track cleanup now happens in onstop handler (after data collection)
      setIsRecording(false)
    })
  }, [])

  const cancel = useCallback(() => {
    cleanup()
  }, [cleanup])

  return { isRecording, duration, start, stop, cancel }
}
