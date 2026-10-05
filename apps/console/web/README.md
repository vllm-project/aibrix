# aibrix-console

This is a bundle for aibrix-console. The original project is available at https://www.figma.com/design/8GNiN53l892dYqaGUt4RJ0/aibrix-portal.

## Running the code

Run `npm i` to install the dependencies.

Run `npm run dev` to start the development server.

## Feature flags

Some in-progress capabilities (online **Deployments** and the **Playground**) are
gated behind build-time feature flags so production builds can stay conservative
while dev/test builds enable them for fast iteration. See
[`src/config/features.ts`](src/config/features.ts) and
[`.env.example`](.env.example).

| Env var                   | Effect                                                        |
| ------------------------- | ------------------------------------------------------------ |
| `VITE_ENABLE_DEPLOYMENTS` | `true` shows the Deployments UI; `false` shows "Coming Soon". |
| `VITE_ENABLE_PLAYGROUND`  | `true` shows the Playground UI; `false` shows "Coming Soon".  |

When unset, both default to **on** under `npm run dev` and **off** in production
builds. To enable a feature for a non-dev build, pass the var at build time:

```bash
VITE_ENABLE_DEPLOYMENTS=true VITE_ENABLE_PLAYGROUND=true npm run build
```
