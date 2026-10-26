# Cadre web UI

The board UI: React, TypeScript and Vite. `npm run build` writes it to
`../internal/web/dist`, which cadre-server embeds and serves at `/`. What it does is in
[docs/architecture.md](../docs/architecture.md#web-ui-and-api), and its design
system (tokens, components, states, accessibility) in
[docs/web-ui.md](../docs/web-ui.md); this file is how to work on it.

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

Vite serves the app with hot reload and proxies `/api` and `/auth` to a
cadre-server on `http://localhost:8080` (`vite.config.ts`), sending them as
that server's own origin. Outside a cluster cadre-server defaults to
`--auth=none`, and the UI asks for a name. To develop with logins, run Dex
as described in docs/architecture.md ("Identity and access") and start
cadre-server with `--auth=oidc`. A login ends at
`http://localhost:8080/auth/callback`, the server's public URL, and lands
there. The session cookie is set for `localhost` and browsers ignore the
port for cookies, so the dev server at `http://localhost:5173` is logged in
too; go back to it.

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
- Who the UI acts as comes from `GET /api/v1/me` (`src/Identity.tsx`). With
  logins it is the user, and actions send no author. With `--auth=none` the
  UI asks for a name once and keeps it in `localStorage`
  (`src/identityStore.ts`). Actions read `useIdentity()` and are disabled
  until it is ready. A 401 from any request sends the browser to
  `/auth/login`.
- Tests use `src/testing.ts` for task and thread fixtures, and stub `fetch`
  with `vi.stubGlobal`; polling tests use Vitest's fake timers.
- Dependencies stay few: react, react-dom and react-router at runtime, plain
  CSS, no component kit.
- Shared pieces are in `src/ui/` (Icon, Badge, Button, Skeleton, Banner,
  Menu, Time, Page); components use the tokens in `src/styles.css`, never a
  raw colour. Icons are inline SVG in `src/ui/Icon.tsx`: nothing is fetched
  at run time, fonts included.
