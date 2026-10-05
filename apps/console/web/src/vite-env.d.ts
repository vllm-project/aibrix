/// <reference types="vite/client" />

// Custom build-time env vars consumed by src/config/features.ts.
// Values are strings (or undefined); parse them through the helpers there.
interface ImportMetaEnv {
  readonly VITE_ENABLE_DEPLOYMENTS?: string;
  readonly VITE_ENABLE_PLAYGROUND?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
