# Cadre web UI

The board UI: React, TypeScript and Vite. `npm run build` writes it to
`../internal/web/dist`, which cadre-server embeds and serves at `/`. Design
notes live in [docs/architecture.md](../docs/architecture.md#web-ui-and-api);
this file is how to work on it.

## Setup

Use the Node version in `.nvmrc` (`nvm use`), then from the repository root:

```sh
make ui          # npm ci and npm run build into internal/web/dist
make ui-check    # typecheck, lint and unit tests
make build       # cadre-server now carries the built UI
```

`make build` alone works without Node: cadre-server then serves a page at
`/` saying the UI is not built.

## Development server

```sh
cd web
npm run dev
```

Vite serves the app with hot reload and proxies `/api` to a cadre-server on
`http://localhost:8080` (`vite.config.ts`). Run one locally against your
cluster, for example with `--runner-auth=none` for development.

## Scripts

| Script | |
| --- | --- |
| `npm run dev` | dev server with the `/api` proxy |
| `npm run build` | typecheck and build into `../internal/web/dist` |
| `npm run typecheck` | `tsc --noEmit` |
| `npm run lint` | ESLint |
| `npm test` | Vitest, in jsdom |

## Notes

- The API is same-origin: requests go to `/api/v1/...`.
- The name the UI acts as (`src/identityStore.ts`, `src/Identity.tsx`) is a
  placeholder until sign-in: it is asked once and kept in `localStorage`.
  Actions read it with `useIdentity()` and are disabled without one.
- Tests use `src/testing.ts` for task and thread fixtures, and stub `fetch`
  with `vi.stubGlobal`; polling tests use Vitest's fake timers.
- Dependencies stay few: react, react-dom and react-router at runtime, plain
  CSS, no component kit.
