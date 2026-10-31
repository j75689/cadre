# Web UI design system

The web UI is an operations console: minimal, dense and grid-based. Typography, spacing and a small set of semantic colours carry the information, not decoration. Light and dark are designed together. There is one accent, and the status colours are used only for status.

It is React, TypeScript and plain CSS (`web/src/styles.css`), with no component kit, no CSS framework and no new runtime dependency. Nothing is fetched at run time: no web fonts and no icon packages. The UI therefore works in an air-gapped cluster. This document is its design system and how each page looks and behaves ([Pages](#pages), [URL parameters](#url-parameters), [States](#states), [Accessibility](#accessibility)); the routes the pages call are in [architecture.md](architecture.md#web-ui-and-api).

## Tokens

The tokens are custom properties on `:root`. Components use the semantic tokens only, never a raw colour.

### Colour

| Token | Light | Dark | Use |
| --- | --- | --- | --- |
| `--bg` | `#f6f7f9` | `#0f1319` | page |
| `--surface` | `#ffffff` | `#171c25` | cards, bars, inputs |
| `--surface-2` | `#eef0f4` | `#1e2430` | column wells, code, hovers |
| `--fg` | `#1d2330` | `#e6e9ef` | text |
| `--fg-muted` | `#5d6678` | `#9aa3b5` | secondary text, numbers, meta |
| `--line` | `#dde1e8` | `#2b3341` | borders, separators |
| `--accent` | `#2f5bd3` | `#8aa6ff` | links, primary buttons, hover borders |
| `--accent-fg` | `#ffffff` | `#0f1319` | text on the accent |
| `--accent-soft` | `#e8eefb` | `#1f2a46` | a pressed toggle, the admin badge |
| `--ring` | `--accent` | `--accent` | focus rings |
| `--shadow` | small | small | hover and menus only |

Every status has three tokens:
- `-dot`: a mark such as a dot, a stripe or a ring.
- `-fg`: text, at 4.5:1 or more on `--surface` and on its own tint.
- `-soft`: the tint behind a badge.

| Status | Hue | Tokens |
| --- | --- | --- |
| Todo | slate | `--todo-*` |
| Running | blue (the dot pulses) | `--running-*` |
| Waiting | amber | `--waiting-*` |
| Review | violet | `--review-*` |
| Done | green | `--done-*` |
| Cancelled | grey | `--cancelled-*` |
| Failed, destructive | red | `--danger-*` |
| Priority urgent | red | `--urgent-*` (the danger tokens) |
| Priority high | orange | `--high-*` |

A component takes a hue through a tone class: `tone-todo` … `tone-failed`, `tone-danger`, `tone-urgent`, `tone-high`, `tone-accent`. The class sets `--tone-dot`, `--tone-fg` and `--tone-soft`, and the badge, dot, chip and kind tag read those. A run's outcome is a phase name (Done, Review, Waiting or Failed), so it takes that phase's tone.

### Themes

`html[data-theme]` is always `light` or `dark`, and the tokens follow it alone: the light ones are the defaults, and the dark ones are defined once, under `:root[data-theme="dark"]`. `color-scheme` follows the theme, so native controls and scrollbars match.

The theme control in the top bar cycles system → light → dark. It keeps the choice in `localStorage` under `cadre.theme`; "system" removes the key. In system mode the attribute is the system's choice (`prefers-color-scheme`, read with `matchMedia`), and it follows the system when that changes. A small inline script in `index.html`, ahead of the stylesheet and the app, sets the attribute from the kept choice or the system's, so a page never paints in the other theme first. Without scripts the page is light.

### Type

- **Sans:** `system-ui, -apple-system, "Segoe UI", Roboto, sans-serif`.
- **Mono:** `ui-monospace, SFMono-Regular, Menlo, Consolas, monospace`. It is used for task numbers, resource names (namespaces), run ids, tool names and code.
- **Scale:** 12 / 13 / 14 / 16 / 20 / 24 px (`--text-xs` … `--text-2xl`).
  - The console's body text is 14 px.
  - Forms, the composer, the thread and the description are 16 px.
  - A page's `h1` is 20 px, or 24 px on a task.
  - Card and column headings are 13–14 px at weight 600.
- **Line height:** 1.5; 1.25 for headings.
- **Numbers:** `font-variant-numeric: tabular-nums`, so counts and times do not jitter.

### Space, radius, elevation, motion

- **Spacing** sits on a 4 px base: 4 / 8 / 12 / 16 / 24 / 32 (`--space-1` … `--space-6`).
- **Radius:**
  - 6 px (`--radius`) for cards, inputs and buttons;
  - 4 px (`--radius-sm`) for small controls and skeleton blocks;
  - 999 px (`--radius-pill`) for badges, pills and counts.
- **Separation:** borders separate things. The one shadow token is used only on hover and for menus.
- **Motion:**
  - Transitions take 150 ms and apply to colour, border and shadow only, never to size or position.
  - The running dot pulses and skeletons shimmer slowly.
  - Under `prefers-reduced-motion: reduce`, every animation and transition is off.

## Layout

### App shell

The top bar is 48 px, on `--surface`, with a bottom `--line`. It holds, in order:
- the brand mark: a 20 px accent square with "C", then "Cadre", linking to `/`;
- the team switcher;
- on the right, the keyboard shortcuts button (a ghost `help-circle` icon button named "Keyboard shortcuts"), the theme control and the identity.

The team switcher is a menu button. It names the current team: the display name, with the namespace in mono beside it. On `/` it reads "Teams". Its menu holds "All teams", then every team from `GET /teams`. When people log in, each team also shows the caller's role on it.

The switcher gives way before anything else in the bar: a long team name truncates with an ellipsis. The identity keeps its size, and a long user name is cut at its own cap (16rem, 30vw under 640 px, 20vw under 400 px, where the identity also drops its icon and Log out its word, keeping its name); an error from reading `/me` truncates, with the whole of it in its title. Under 640 px the brand's name and the namespace are hidden, the switcher is at most 40vw, and the name form takes the room the bar has left. The bar never scrolls sideways, down to 320 px.

The identity shows:
- with logins: the user's name and a "Log out" ghost button;
- with `--auth=none`: "acting as" the name, with "change";
- while no name is set: the name form, inline.

A skip link to `#main` comes first in the tab order. Each page's `main` has `id="main"`.

#### Team nav

Every team page, `/t/<ns>/...`, has the team nav under the top bar: a `nav` named "Team", 40 px (`--teamnav`), on `--surface`, with a bottom `--line`.
- It holds the team's tabs, each an icon and a label, in order: **Board**, current on `/t/<ns>` (with or without a trailing slash) only, not on a task's page; **Agents**, for every role; **Model providers**, for every role, also current on a provider's page; **Settings**, for every role, also current on its sub-pages (`/t/<ns>/settings/access...`).
- The current tab has `aria-current="page"`, a 2 px `--accent` underline and `--fg` text; the others are `--fg-muted`.
- Every role sees all four tabs. What a role may not do is left out of the pages themselves (see [States](#states)).
- When the tabs do not fit, they scroll inside the nav; the page never scrolls sideways, down to 320 px.
- An unknown team page, `/t/<ns>/<anything else>`, shows Not found under the nav.

#### Keyboard shortcuts

`useShortcuts` (`web/src/shortcuts.tsx`), called once in the layout, listens for single keys on the document:
- `?` opens the help, `HelpDialog`: a `Dialog` titled "Keyboard shortcuts" with the shortcuts in a `dl`; `c` is listed only for a member or an admin of the team. The top bar's "Keyboard shortcuts" button opens it too.
- `/` focuses the board's search (`#board-search`), where there is one.
- `c` opens New task (`/t/<ns>/new`), on a team's pages, for a member or an admin. On New task already, it does nothing, so the history gets no second entry.
- `g` then `b` within 1 s goes to the team's board (`/t/<ns>`). On the board already, it does nothing, so the board's search and filters stay.

A shortcut is a key without Ctrl, Cmd or Alt, which belong to the browser and the system. For `/` and `?` only, AltGr is no modifier, though Windows sends it as Ctrl and Alt, so they work on layouts that type them with AltGr. None fires when another handler has prevented the key, while the focus is in an `input`, `textarea`, `select` or `[contenteditable]`, during an input method's composition (`isComposing`, or `keyCode` 229), or while a dialog is open, except `?`; Escape closes the dialog. `c` and `g` `b`, which leave the page, also do not fire while the focus is in a `form`, or while the page holds a draft, wherever the focus is. A draft is an element marked `data-dirty`: New task's form once it differs from a blank one, the team's Settings form while it would save something other than the Team it opened with, an agent access form while it would save something other than what it opened with, a task's reply or comment while it holds text, and a tool call note, also while Comment instead hides it. Each form sets the mark itself, as React keeps a controlled field's `defaultValue` equal to its value, so a field cannot tell it was changed. After `c` and `g` `b`, the new page's `h1` takes the focus, as after any route change.

### URL parameters

The team's URLs and their query parameters are built in `web/src/links.ts` (`params`, `teamPath`, `boardLink`, `taskLink`), so a link from one page is one another page reads. A page's state that a link should carry is in its query, so a reload or a shared link opens the same view:

| Parameter | Page | Value | Set by |
| --- | --- | --- | --- |
| `q` | Board | The search text, matched in any case against the title, the task's name, the assignee, the labels and `#number`, over the loaded tasks. | the search box |
| `phase` | Board | One phase, in any case (`todo` is Todo): only that column is read and shown. An unknown phase reads as all of them and is left in the URL. | the Phase select |
| `assignee` | Board | An assignee's exact name; a User and an Agent of the same name are one assignee. Filters the loaded tasks. | the Assignee select; Agents' Tasks link (`boardLink(ns, { assignee })`) |
| `view` | Board | `list` for the list; any other value is the board. | the Board and List toggles |
| `sort` | Board, as a list | `number`, `title`, `phase`, `assignee`, `priority` or `created`, with `-` first for descending. None, or an unknown one, is the API's order, priority then number. | the list's header buttons |
| `run` | Task | A run's id, in any case: the page opens at that run, expanded if it has ended. | a run's id link in Runs and Run in progress; Settings' Run slots (`taskLink(ns, name, run)`) |

The board's controls replace the history entry, so they do not flood Back, and an empty value removes its parameter. A change of the query alone keeps the focus where it was.

### Pages

- **Teams** (`/`): a card grid of `repeat(auto-fill, minmax(16rem, 1fr))`. Each card shows the display name, the namespace in mono and, with logins, a role badge. The whole card links to the board.
  - **Create a team:** for a bootstrap admin (`me.admin`, which is everyone under `--auth=none`), the empty state is a form card, at most 40rem wide: Namespace (required, mono) and Display name. The namespace is checked before the request as the server checks it: a DNS-1123 label of at most 63 characters. "Create team" PUTs the namespace's Team without a version, which creates it, then the board opens. A namespace that already has a Team says so, with a link to it; a namespace that isn't in the cluster (the server's 404, whose message is just "not found") says to create it first, with the `kubectl create namespace` command; any other error shows under the form in a `role="alert"`. When the error is the namespace's (the check before the request, or the 404), the field is `aria-invalid`, described by the alert, and takes focus. The form is inline on `/`, as the server serves no other top-level route.
  - With teams, a bootstrap admin also has "New team" in the page head, a toggle (`aria-expanded`) that shows the same form above the grid.
- **Board** (`/t/<ns>`):
  - **Toolbar:**
    - `h1` (the team's name) with the role badge;
    - the live status;
    - the search field (icon, clear button), `id="board-search"`;
    - the filters, two selects with visually hidden labels: **Phase** ("All phases" and the seven phases) and **Assignee** ("Anyone", the assignees of the loaded tasks, and the one in the URL even if no loaded task has it);
    - the view, a group of two toggle buttons (`aria-pressed`): **Board** and **List**;
    - "n of m loaded tasks shown" while a search or an assignee is set: both filter the pages already loaded, not the server;
    - "New task", the one primary button, for members.
  - **URL:** the search, the filters, the view and the list's order live in the query (`q`, `phase`, `assignee`, `view`, `sort`; see [URL parameters](#url-parameters)), so a reload or a shared link opens the same board. The search box keeps its own value and writes it to `?q=` (`useTextParam` in `web/src/links.ts`), so fast typing and an input method's composition are not lost to the router's transition: it writes when a composition ends, not during it, and takes the URL's value on Back, Forward or a link. With a phase set, only that column is read and shown, at most 32rem wide from 1024 px. A filter change keeps the focus on its control.
  - **Columns:** a horizontal scroller.
    - Under 1024 px it snaps, and each column is 80 vw.
    - From 1024 px there are seven columns of `minmax(15rem, 1fr)`.
    - Each column is a `--surface-2` well with a sticky header: a dot in the phase's hue, the name, and a count pill (`50+` while there is more).
    - The column body scrolls on its own, so headers stay in view. The board fills the viewport below the top bar and the team nav.
    - Terminal columns (Done, Cancelled, Failed) mute their header.
    - The phases are read in parallel, so a task that changes phase between two reads comes back in both. It is shown once, in the column whose read came back last (board and list alike).
  - **List** (`?view=list`): the loaded columns' tasks, filtered as the board is, in one table in a region named "Tasks": #, Title (a link to the task), Phase (the phase badge), Assignee, Priority (medium shown too) and Created. A task has no updated time, so there is no Updated column.
    - Each header is a button in its `th`; the sorted one has `aria-sort`. A click sorts ascending, then descending. The key is kept in `?sort=`: `number`, `title`, `phase`, `assignee`, `priority`, `created`, with `-` first for descending. No key (or an unknown one) is the API's order, priority then number, so Priority then has `aria-sort="ascending"`, and choosing that order removes the key; the number breaks ties.
    - One "Load more" below the table reads the next page of every column that has more, the board's paging.
    - The table fills the board's height and scrolls on its own, both ways, so the page does not; its header stays in view.
- **Agents** (`/t/<ns>/agents`): the team's Agent resources and whether each can take work, read every 15 s. It is health only, so every role sees it.
  - A table in a horizontal-scroll region named "Agents": Agent (mono, a link to its page, `agentLink(ns, name)`), Status (the ready badge), Reason (muted, empty while Pending, which the badge says), Message, and Tasks, a link to the board with the agent as the assignee (`boardLink(ns, { assignee })`), which opens the board filtered to that agent.
  - With no agents: "This team has no agents. Agents are Agent resources in the namespace, applied with kubectl or GitOps."
  - An error that stops the read shows the banner with Retry.
- **One Agent** (`/t/<ns>/agents/<name>`): the Agent as `GET /agents/<name>` answers it for the caller's role, read every 15 s, read-only (GitOps owns it). The Agents tab stays current.
  - Reach it from the Agents table, by an Agent's name, or by its address: `/t/<ns>/agents/<name>` opens it directly, also in a new tab or from a bookmark. It has no shortcut of its own; `c` and `g` `b` work here as on any team page ([Keyboard shortcuts](#keyboard-shortcuts)).
  - By role: a viewer sees the header, Conditions, Metadata (without annotations) and a YAML of `metadata` alone. A member also sees Uses, the annotations and the `spec` and `status` in the YAML, with a job template's env values, command and arguments as `(set)`. An admin (everyone under `--auth=none`) sees those whole.
  - The breadcrumb is the team, Agents, then the name; the `h1` is the name, then the kind and the ready badge. Under it, the reason (not when Pending, which the badge says, as the server decides it) and the message.
  - Cards, each a `section` named by its `h2`, built from `ui/Config.tsx` so the other kinds' pages can reuse them:
    - **Conditions**: a table in a horizontal-scroll region named "Conditions table": type, status, reason, message, last transition, and the generation it is about, "*observed* of *generation*", with "(older spec)" when it is about a spec since changed. With none: "No conditions yet: the controller has not reported on it."
    - **Uses**: the ModelProvider (`spec.modelRef`), a link to its page (`modelProviderLink(ns, name)`), and the ToolServers (`spec.toolRefs`), as names until their page exists (`Ref` in `AgentDetail.tsx` is where they become links). A viewer, who gets no spec, has no Uses card but "The spec and status are shown to the team's members and admins."
    - **Metadata**: created, generation and observed generation, labels, annotations when the answer has them (members and admins), uid and resource version.
    - **YAML**: the answer's `metadata`, `spec` and `status`, whichever it has, printed as YAML (`toYAML` in `yaml.ts`, no dependency; it prints the JSON, it is not `kubectl get -o yaml`: there is no `apiVersion` or `kind` line, and no `managedFields`, so it is not a manifest to `kubectl apply` as it is), in a focusable `pre` named "YAML text" that scrolls both ways. Copy uses `navigator.clipboard.writeText`, then "Copied" in a `role="status"`; without a clipboard the text is selected and the status says "Press Ctrl+C to copy it." Values a member is shown masked stay `(set)` as served; when the YAML holds one, whole, or a ModelProvider's `spec.baseURL` holds one as its user info, a query value or its fragment (`maskURL` in `internal/api/config.go`), "Values shown as `(set)` are set but hidden from members; admins see them." A `(set)` elsewhere in a text, even one that looks like a masked URL, is an admin's as written and adds no note.
  - An Agent that is not there (a 404 with the code `not_found`) reads "No agent named *name* in this team." with a link to All agents. A failed read shows the banner with Retry; a 403 (`no_role`) shows it without Retry.
  - What uses the Agent (the Schedules that target it) is not shown yet: it needs the Schedule API.
  - The page is `ConfigDetail.tsx`, any kind's; the Agent adds its Uses card. The list is `ConfigList.tsx`, which the Agents page fills with its Tasks column.
- **Model providers** (`/t/<ns>/modelproviders`): the team's ModelProvider resources and whether each is ready, read every 15 s, for every role: the Agents list without its Tasks column, in a region named "Model providers", each name a link to its page (`modelProviderLink(ns, name)`). With none: "This team has no model providers. Model providers are ModelProvider resources in the namespace, applied with kubectl or GitOps." A failed read shows the banner with Retry.
- **One ModelProvider** (`/t/<ns>/modelproviders/<name>`): the ModelProvider as `GET /modelproviders/<name>` answers it, laid out as One Agent (breadcrumb, header, reason and message, Conditions, Metadata, YAML, and the same 404, 403 and error states, "No model provider named *name* in this team." with All model providers) but with no Uses card: its spec (type, model, auth, base URL) is read in the YAML. A viewer gets health and metadata only; a member's `spec.baseURL` keeps the `(set)` parts as served, with the note; an admin's is whole.
  - The Agents that use it (`spec.modelRef`) are not shown: the Agents list is summary-only and a viewer gets no Agent spec, so it needs an API that answers them (`TODO(used-by)` in `ModelProviders.tsx`).
- **Task** (`/t/<ns>/tasks/<name>`):
  - **Header:**
    - a breadcrumb: the team, then `#number`;
    - `h1`: the number in mono, then the title;
    - the phase badge and the priority chip;
    - on the right, "Cancel task" as a danger ghost button, which opens an inline confirmation.
  - **Body:** a grid of `minmax(0, 3fr) minmax(18rem, 1fr)`, stacked under 800 px.
    - The main column holds Description, Thread, the composer and Subtasks.
    - The side column holds Details, the run in progress (while Running) and Runs.
  - `?run=<id>` (any case) opens the page at that run: the run in progress, or an ended run, expanded with its events read. It is scrolled into view once each time it is linked, after the thread, the subtasks and the runs have first been read, or failed to be (under 800 px the thread and the subtasks are above it, and the runs below make the page long enough): on load, to the top, or as near it as the end of the page lets it go; from a run's link on the page, or by Back and Forward, only as far as it takes, so the id of a run already in view does not move the page. On Back and Forward it is scrolled again after the browser has put the entry's scroll position back. A linked run closed with its button opens again when its link is clicked. Event lines wrap anywhere, so a long path or URL does not widen the page.
- **New task** (`/t/<ns>/new`): one form card, at most 40rem wide.
  - Labels sit above fields, and inputs are 16 px.
  - Assignee and priority are segmented controls.
  - "Create task" is the primary button; "Cancel" is a link back to the board.
- **Settings** (`/t/<ns>/settings`): the team's Team, read every 15 s, at most 60rem wide.
  - A breadcrumb (the team, then Settings), then `h1` "Settings" and, with logins, the caller's role.
  - Under the heading, the settings' tabs: a `nav` named "Settings" with **Team** (`/t/<ns>/settings`, current there only) and **Agent access** (`/t/<ns>/settings/access`), shown only with logins (`--auth=oidc`) to an admin of the team. The current tab has `aria-current="page"` and a 2 px `--accent` underline; the tabs scroll inside their bar when they do not fit.
  - **Team** card: a key/value list of the display name (the namespace, muted, when there is none) and the namespace in mono. For an admin (everyone under `--auth=none`) it also shows the max concurrent runs and the archive-after duration; an unset cap reads "1 (default)", as the server runs one task at a time then, and an unset duration reads "never".
  - **Editing**, admins only (everyone under `--auth=none`): "Edit" in the Team card's header opens a form in place of its key/value list, with the focus on its first field: the display name; the max concurrent runs (a whole number from 0 to 2147483647; empty or 0 is saved as 1; what the browser cannot read, such as `1e`, is an error); the archive-after duration (a positive Go duration such as `168h`, `30m` or `.5h`, at most Go's `2562047h47m16.854775807s` (compared exactly, as a `BigInt`), checked before sending, with its own message when too long; empty is never); and the roles, each a role (admin, member or viewer) and its subjects, each a user/group `Segmented` and a name, with "Add subject", "Remove role" and "Add role"; after "Remove role" the focus goes to the role now in its place, else the one before, else "Add role". A role needs a subject and a subject a name; errors number roles and subjects as the form does ("Role 2, subject 1"), the server's 0-based `roles[1].subjects[0]` included. The draft is copied once from the Team when the form opens; the 15 s reads do not change it.
    - "Save" sends the whole spec with the `resourceVersion` it was made from. On success the card shows the saved values, "Saved" is announced (`role="status"`), and the focus goes back to "Edit", as it does on "Cancel".
    - A 409 `conflict` (someone else saved first) shows a warning banner: "Someone else changed the team since it was read. Their current values are shown below, and the fields they changed are marked. Saving again replaces their values with the ones in the form." The Team is read again and shown as "Current" above the form; each field whose current value differs from the one the form opened with, and from the form's, gets "Changed by someone else: *value*" (`aria-describedby`). The user's edits are kept, nothing is merged, and the next save carries the new version. If the Team is gone (a bootstrap admin, whose role does not come from it, then reads a 404), Save is disabled; when a 15 s read finds it gone while the form is open, the form closes and "This namespace has no Team" takes the focus. Another 409 is shown as an error.
    - A 403 `no_role` on Save, or on a read once the Team was read: the Team was deleted (a role given by the Team goes with it, so the server answers 403, not 409 or 404) or the caller's role was taken away. The form closes, the page drops the Team it holds and says "Your role on *ns* is gone or has changed: its Team was deleted, or someone changed its roles." (a focused alert, instead of "Could not load the team"), reads `/me` again, so the role line is current, and reads the Team again: what the caller may still read shows, without Edit or the admin cards, until the page is opened again.
    - When a save takes away the caller's own admin role, the server answers with the Team as a non-admin sees it: the page says "You no longer have the admin role on *ns*, so its limits, roles and run slots are hidden." (focused), hides them and Edit, and reads `/me` again.
    - Any other refusal (a 400, such as an edit that would leave the team without an admin) is an alert right after the Roles fieldset, beside Save. Alerts take the focus, each failed Save again, also with the same message (the alert is remounted, so it is announced again). The field an error is about (from the check before sending, or the server's `maxConcurrentRuns`, `archiveAfter`, `roles[i]` or `roles[i].subjects[j]` prefix) is `aria-invalid` and described by the alert; for a role without subjects, its fieldset is described by it.
    - A Team that looks managed by GitOps (`gitops.ts`: a `kubectl.kubernetes.io/last-applied-configuration` or `argocd.argoproj.io/tracking-id` annotation, an `app.kubernetes.io/instance`, `kustomize.toolkit.fluxcd.io/name` or `helm.toolkit.fluxcd.io/name` label, or a server-side `Apply` in its managedFields) gets a warning banner at the top of the form (its `aria-describedby`), saying why; the first Save then asks "Save anyway?" inline (Save anyway / Keep editing).
    - Warnings use `.banner-warning`, in the waiting tone; errors stay red.
  - **Roles** card, admins only: a table of each role (`RoleBadge`) and its subjects, as `user:<name>` or `group:<name>` in mono, in a `.table-text` cell, so they wrap; with none, "No roles are given on this team."
  - **Run slots** card, admins only: "*running* of *max* in use" in its header (*max* is 1 when unset), then a table of the slots held: the task (a link to it, opened at the run), the run id in mono, and when the slot was acquired; with none, "No run holds a slot."
  - The server sends a viewer or a member the Team without its roles and status. It does send the limits, but the page shows them to admins only. So the others see the display name and the namespace, and the hint "This page shows a team's limits, roles and run slots to its admins only."
  - A namespace without a Team shows "This namespace has no Team." with the `cadre team init` command. A failed read shows a banner with Retry; a 403 shows it without Retry, as reading again cannot help.
- **Agent access** (`/t/<ns>/settings/access`): the team's AgentAccesses, read every 15 s, at most 60rem wide, under the same breadcrumb (the team, Settings, then Agent access), heading and settings' tabs.
  - The server serves AgentAccesses with logins only, to a team's admins (with `--auth=none` the routes are not registered). So the page waits for `/me` and requests nothing without logins or for a caller who is not an admin: under `--auth=none` the page says "Agent access needs logins. This cadre-server runs with --auth=none, so it serves no AgentAccess routes; use kubectl, or run it with --auth=oidc."; a viewer or a member reads "Managing agent access needs the admin role.". When `/me` failed, the server decides: a plain 404 (the router's, with no error code) then shows the same "needs logins" card, not an error. Under `--auth=oidc` the routes are always served, so a plain 404 there comes from something in front of the server and shows the banner with Retry, like any failed read.
  - A table of each AgentAccess: its name (mono, a link to its page; it wraps, at most 16rem), its role (`RoleBadge`), its credential ("ServiceAccount *name*", "Minted token in Secret *secret*" and when it was minted, or "Token not minted yet"), its readiness (`ReadyBadge`), the reason (blank for Pending, which the badge says) and the message, in a `.table-text` cell. None reads "No AgentAccess yet.". A failed read shows a banner with Retry; a 403 shows it without Retry.
  - **New agent access** (a primary button above the table) opens a form in a card, "New agent access": Name (mono; the same DNS-label check as the page's, at most 63 characters, else "A name is at most 63 lower-case letters, digits and '-', and starts and ends with a letter or a digit." and nothing is sent), Role (`Segmented`: viewer, member; member by default) and Credential (`Segmented`: Minted token, the default, with "The reconciler mints the token into a Secret.", or ServiceAccount, with a ServiceAccount name field, required). Create sends `putAgentAccess(ns, name, spec)` without a version, so `{"spec":{"role":"member"}}` or `{"spec":{"role":…,"serviceAccount":{"name":…}}}`; a 201 opens the AgentAccess's page. Any refusal (a 409 `already_exists`, a 400) shows the server's message as it is in an alert under the fields, which takes the focus; the form keeps what was typed. Cancel closes the form.
  - **One AgentAccess** (`/t/<ns>/settings/access/<name>`): the breadcrumb ends in Agent access, then the name; the `h1` is the name. A card lists the name, role, credential, status, reason and message when set (no Reason row for Pending, which the badge says), and when the token was minted. A minted token, unless the AgentAccess names a ServiceAccount, adds "The token is never shown again after it is minted; read it from Secret *secret*, key `token`, with `kubectl -n <ns> get secret <secret> -o jsonpath='{.data.token}' | base64 -d`, or rotate it to see a new one here."; one not minted yet adds "The reconciler mints the token into a Secret; rotate it to see one here." An AgentAccess that is not there (a 404 with the code `not_found`) reads "No AgentAccess *name*." with a link back to the list; so does a name that is not a DNS label of at most 63 characters, which the CRD refuses, and it is not requested. A failed read shows a banner with Retry; a 403 shows it without Retry.
    - The card's head holds Edit, Rotate token (only when the AgentAccess names no ServiceAccount) and Delete, and a status (`role="status"`) that says "Saved" or "New token shown below", never the token.
    - **Edit** puts the same form, "Edit *name*", in place of the details, the name read-only, under "If AgentAccesses are applied from Git, change them there: an edit here drifts from Git.". The draft is made once, when Edit is pressed: the 15 s reads do not touch it. Save sends `putAgentAccess(ns, name, spec, resourceVersion)` with the version the form opened with; a 200 shows the AgentAccess saved. A 409 reads the AgentAccess again. If its spec is still the one the form last read, only its status moved (the reconciler writes it after each save and rotation, which moves the version on), and the form saves again at the new version without showing the 409. Otherwise someone else changed it: the form shows the server's message as it is, keeps the draft, and the next Save carries the new version. Other refusals show the server's message as it is. Edit puts the focus on the role picked, as the name cannot be changed; Cancel and a save put it back on Edit.
    - **Rotate token** asks inline (`.confirm`, a group named "Rotate the token?"): "Rotate the token? The old one stops working at once." (Yes, rotate it / Keep it). A 200 shows the token once, under the details: a read-only mono field named "New token" (`autoComplete="off"`, `spellCheck={false}`, focused), Copy (`navigator.clipboard.writeText`, then "Copied" in a `role="status"`; without a clipboard, as on a plain-HTTP origin, the token is selected and the status says "Press Ctrl+C to copy it."), "This is the only time this token is shown. It is also in Secret *secret*." and Done, which clears it. A refusal (a 409, a 400) shows the server's message as it is in an alert, with no token field: some 409s mean a token was minted into the Secret but is not shown.
    - The token lives only in the AgentAccess card's React state: never in a context, `localStorage` or `sessionStorage`, the URL or history state, the title, an `Error`, the console or a live region. Leaving the page drops it, and so does `pagehide` before the browser keeps the page in its back/forward cache, so Back cannot show it again. cadre-server also sends `Cache-Control: no-store` with the app's pages and with the rotation's answer: Chrome then keeps a page that rotated a token out of its back/forward cache, and Back loads it again. A page that rotated nothing is still restored there, unless Log out changed the session cookie meanwhile. A rotation answered after the page was left shows no token either: the card says "Rotated while the page was left, so the token is not shown: it is in Secret *secret*; rotate again to see one here". An AgentAccess found gone drops the token, an open Edit form and a pending confirm, so one made again with the name brings none of them back.
    - **Delete** asks inline (a group named "Delete *name*?"): "Delete *name*? Its agent loses access at once." (Yes, delete it / Keep it). A 204, or a 404 (already gone), goes back to the list, whose status mounts empty and then says "Deleted *name*.", so that it is announced. Any other refusal shows the server's message as it is in an alert.

### Live updates

The pages of a team read what they show again as soon as cadre-server hints that it changed, on its [watch stream](architecture.md#events) (`web/src/live.ts`). A tab holds one stream per team, shared by its pages, opened by the first page that wants it and closed when the last one goes, so leaving the team closes it. Only the task page's run in progress opens it with `?task=<name>`, so a page holds one stream at most; another task reopens it. The stream only says what to read: each page reads with the route and the role it reads with anyway.

| Hint | What is read again |
| --- | --- |
| `task` | The board and its list, every task; the task page, its own task; its Subtasks, a task whose parent it is |
| `config` | The Agents and Model providers lists, a resource of their kind; one Agent or ModelProvider, itself; Settings, the Team |
| `run` | The run in progress's events, its own run |
| `resync`, or a `hello` after a reconnect | Everything the tab shows from the stream, as hints in between were lost |

A hint reads 100 ms after it comes, so hints that come together are one read, and a read already under way finishes first and is followed by one more: a change shows within a second. While a page is live it still reads every 60 seconds (or its own interval if longer), as hints may be lost. The Agent access pages and the team list get no hints and poll as before.

A page falls back to its interval (the board, the task page and its subtasks every 5 s, the run in progress every 4 s, the other pages every 15 s) whenever its hints do not come:
- the stream is refused when opened: `401`, `403`, `404` (a namespace without a Team, or a cadre-server without the route) or `503` (the replica's cap). The browser does not retry these, and the tab polls until the team is opened again;
- its `hello` says the topic is not served (`tasks`, `config` or `runs` false);
- it ends with `no_role` or `no_team`: it is not opened again, and the next poll shows the 403 or the 404 as the page does today;
- it drops: the browser reconnects every 3 s, and the page polls until the next `hello`. `end` `lifetime` or `shutdown` is such a reconnect.

A browser without `EventSource` polls, as every page did before the stream.

## Components

Shared pieces live in `web/src/ui/`.

| Component | What it is |
| --- | --- |
| `Icon` | Inline SVG, 16 px, 1.5 px stroke, `currentColor`. The shapes are Lucide's, and its ISC licence is in the file header. It is `aria-hidden` unless it stands alone with a label. |
| `Button`, `ButtonLink` | Variants: primary, secondary, ghost, danger (outline), danger-ghost, danger-solid, success. `small` gives a 28 px control. An icon-only button has an `aria-label`. |
| `Badge`, `PhaseBadge` | A pill: text in `-fg` on `-soft`. A phase badge has a dot in the phase's hue and the phase's name; a Running one pulses. |
| `PriorityChip` | Urgent and high as an alert icon and the word, in their hue. Low as muted text. Medium, the default, shows nothing on cards and "medium" where the priority is always shown. |
| `ReadyBadge` | Whether a resource can take work, as an icon and a word: Ready (done tone, check), Pending while its current spec is unchecked (waiting tone, hourglass), otherwise Not ready (danger tone, alert). |
| `RoleBadge` | viewer, member or admin (accent tint). |
| `Pills` | Labels: bordered pills that wrap. |
| `Meta` | A row of an icon and text: an assignee (bot or user), "subtask of", a wait. |
| `Skeleton`, `SkeletonCard`, `Loading` | Placeholder blocks. `Loading` wraps them, is `aria-busy`, and carries a visually hidden "Loading …" for screen readers. |
| `Banner` | An inline error: an alert icon, the message and, where a retry exists, Retry. With `banner-warning`, a warning in the waiting tone. |
| `Segmented` | A radio group in a `fieldset`, drawn as one row of buttons, with its `legend`. |
| `Menu`, `MenuNote` | A menu button. It opens a `role="menu"` list of links and focuses the first item. Arrow keys, Home and End move between items, and Escape closes it and returns focus to the button. A click outside, Tab or a choice also closes it. A line that is not a choice (the teams still loading, or failing to load) is a `MenuNote`: a disabled menu item, so assistive technology reads it; it is not highlighted on hover or focus, and choosing it does nothing. |
| `Time` | A `<time>`: relative under a day ("just now", "12 s ago", "5 min ago", "3 h ago"), otherwise the full date. The full date is always in its `title`. |
| `Dialog` | A modal: a native `<dialog>` opened with `showModal()`, so Escape closes it, the page behind is inert and it sits in the top layer. Props `open`, `onClose`, `title` (its `h2`, which names it through `aria-labelledby`) and `children`. A ghost "Close" icon button is in its head. On `--surface` with a `--line` border and the 6 px radius; its `::backdrop` is `--bg` at 40 %. When it closes, the focus goes back to the element that had it when it opened, or to the page's `h1` if a route change took that element away. |
| `Heading`, `useTitle` | A page's `h1`, which takes focus after a route change (not after a change of the query only), and its document title. |

### Tables

A list of resources is a `.table` inside a `.table-wrap`: the wrapper scrolls sideways (`tabindex="0"`, `role="region"` and an `aria-label`), so the page never does, down to 320 px.
- Full width, tabular numbers, a 1 px `--line` under each row but the last.
- Header cells are left-aligned, 13 px, weight 600, `--fg-muted`. The header does not stick: the wrapper is the scroll container, so a sticky header would never move.
- Cells align to the top, so a row with a wrapped cell reads from its first line.
- Cells keep to one line; a `.table-text` cell, such as a message, wraps at 16rem or wider.

### Board cards

A card is `--surface` with a 1 px `--line` border and a 6 px radius. On hover the border turns `--accent` and the card gets the shadow.
- **Top row:** a pulsing blue dot while Running, then `#number` in mono (muted; "new" while unnumbered), then the priority chip.
- **Title:** weight 600, clamped to two lines. The title link covers the whole card. The "subtask of" link sits above that cover as a separate link, since links cannot nest.
- **Meta rows:** the assignee (bot or user icon), the parent (branch icon), and while Waiting, "Waiting: <reason>" with the hourglass, in amber.
- **Labels:** pills.

A Waiting card also has a 3 px amber left stripe. An empty column reads "Nothing here", and "Load more" is a full-width ghost button.

### Thread

The thread is a timeline: a 1 px rail with an author glyph (bot or user) on it for each entry. Each entry's head shows:
- the author's name;
- the kind tag, an icon and a word:
  - comment
  - question (amber)
  - answer
  - tool call
  - approved (green)
  - denied (red)
  - tool result;
- the time.

Bodies are plain text, `pre-wrap`, at 16 px. Markdown is not rendered.

A tool call's arguments sit in a `<details>`, closed by default and summarised by the tool's name, with "failed" in red when the result was an error.

The entry the task waits on gets an amber ring and tint, a "Waiting on this" badge and `aria-current`. It is scrolled into view once for each wait, the first time it is there: a later wait, on a later entry, is scrolled to in its turn, and polls do not scroll again. An empty thread reads "Nothing in the thread yet."

### Composer

The composer is one card at the end of the thread, and what it shows depends on what the task waits for:
- **Waiting on a question or a review:** a Reply form, with a hint saying which.
- **Waiting on a tool approval:** an approval card. It names the tool call (from the entry waited on), takes an optional note, and offers "Approve" (success) and "Reject" (danger outline).
- **Otherwise:** a Comment form.

While it offers a reply or an approval, a "Comment instead" toggle button (`aria-pressed`) turns it into the Comment form and back, so a comment can be made in every phase the API takes one. The toggle stays the same element, so it keeps focus. What is typed belongs to the wait it was typed for, held by the composer (not one form), so it survives the toggle both ways. A wait is known by when it started, which stays as its entry is filled in a poll later. When the task moves:
- **to another wait** (another question, another tool call, even of the same kind): a reply goes with its question, so the box is emptied, unless "Comment instead" was pressed, when the comment and the pressed toggle stay;
- **from running to a wait:** a comment being typed stays a comment, so the toggle is pressed for it; with nothing typed, the composer offers the reply or the approval;
- **from a wait to running** (someone else answered): the text stays, in the Comment form.

The same wait closed and opened again (a role refused, then given back) keeps all of it. A reply that was sent passes nothing on. The approval's note and a form's error always go with their wait, so a note meant for one call is never sent with the next.

A Reply form's hint (what the task waits for) is outside its label and describes the text box (`aria-describedby`), so the box is named "Reply".

Buttons are disabled while a call is in flight, and the server's error shows under the form in a `role="alert"`.

### Details and runs

- **Details:** a key/value list:
  - the phase badge;
  - while waiting: the reason as an amber chip, with its since-time;
  - the assignee with its icon;
  - the priority chip;
  - labels;
  - the run in progress (mode, runner, step, attempt, lease);
  - the run count;
  - conditions, each with a check (`True`) or an alert icon, its reason and its message.
- **Run in progress:** shown while the task is Running. The id in mono links to the run (`?run=<id>`), then the mode, runner and step, and the token totals: "12,345 in · 2,100 out · context 30,120", plus the prompt cache's read and written tokens when there are any. In and out are summed over the events; context is the latest event's, the size of the conversation, left out until an event reports it. Under them the events, as in Runs, read again every 4 s, counted from the start of the read before and at least 0.8 s after it ended, while the tab is visible, so an event is shown within 5 s of being written while a read takes up to 1 s; "No events yet." until the first. The card goes when the run ends, and Runs reads its list again.
- **Runs:** each run is a row button: a chevron, the mode, runner and reason, and an outcome badge in the outcome's phase hue, with the start and end times under it. The id in mono sits above the button, a link to the run named "Run <id>", as wide as the id. Expanding a row reads its events once, as an ended run's do not change, and shows them in tabular mono: progress lines and token counts, with the prompt cache's when there are any.

## States

| State | How it looks |
| --- | --- |
| Loading | Skeletons shaped like the content: three team cards, seven columns of two cards, the task's header and cards, a runs line. Never the word "Loading…" on screen. |
| Error | An inline banner where the content would be, with Retry where a retry exists. Once the board has loaded, a failed poll keeps its columns and shows the error in the live status: "Could not refresh: <message>". A refused action's reason stays when the role is read again and the action goes, for the composer and for "Cancel task" alike, and it is said once: a refusal for the role is not also shown under the form, and its alert stays the same element as the action it was for goes: the composer's card stays to hold it. |
| Empty | Says what would fill it: "No team yet", with `cadre team init -n <namespace>` in a code block (for a bootstrap admin, the create form, with the command as the other way); "Nothing here" in a column; "Nothing in the thread yet."; "No run has ended yet." |
| Not found | A centred card with a link back to Teams. |
| No Team | A namespace without a Team is no team, and `/` does not list it (with no teams at all, see Empty). Its Settings says "This namespace has no Team. A namespace becomes a team when it has a Team resource:" with `cadre team init -n <ns>`. |
| Without logins | With `--auth=none` there is no login: `/me` makes everyone an admin, so every action and Settings' Edit are offered, no role is shown, and the identity is a name kept in the browser (a task's composer says "Give your name at the top to act on this task." until there is one). Agent access says it needs logins, as the server serves no AgentAccess routes then. |
| Role | The UI leaves out what the caller's role would be refused; the server decides. With logins, the board, a task, New task, Settings and Agent access say the role under their heading ("You are a viewer of *ns*." or "You have no role on *ns*."). A viewer gets no "New task", composer, "Cancel task" or `c`; New task itself says "Creating tasks needs the member role.". A viewer or a member sees Settings' display name and namespace with "This page shows a team's limits, roles and run slots to its admins only.", no Edit and no Agent access tab, and an Agent access URL says "Managing agent access needs the admin role.". A role lost meanwhile is said where it shows: Settings' "Your role on *ns* is gone or has changed …", and a refused action's reason (see Error). |

## Accessibility

- **Contrast:** text is at 4.5:1 or more in both themes. That includes `--fg-muted` on `--surface`, `--bg` and `--surface-2`, every status `-fg` on `--surface`, `--surface-2` and its `-soft` tint, and text on the accent and on the success and danger buttons. Change a token only after checking it again.
- **Focus:**
  - Every control and card has a 2 px `--ring` `:focus-visible` outline. A card's ring is drawn by its covering link.
  - After a route change, focus moves to the new page's `h1` (`tabindex="-1"`). The first page of a visit keeps the browser's focus, and so does a change of the query only (a run linked, Back and Forward between runs), so focus stays where it was and the title is not read again.
- **Names:**
  - Icon-only controls (keyboard shortcuts, theme, clear search, a dialog's Close) have an `aria-label`.
  - Decorative icons and skeleton blocks are `aria-hidden`.
  - A Running card's dot is an `img` named "running".
- **Colour alone:** phase and priority never rely on colour only: a phase is a dot and its name, and a priority is an icon and its word. A wait is an icon and the word "Waiting".
- **Structure:** one `h1` per page. Landmarks: `header`, `main` (skip-link target), the breadcrumb, Team and Settings `nav`s, and `region`s for the board's columns, the task's cards and each scrolling table.
- **Live regions:** the board has a single polite `role="status"` with `aria-atomic`. Only a change of state reaches it: "Up to date" (visually hidden) or "Could not refresh: <message>". How long ago the board was read ("Updated 12 s ago") is shown beside it, outside the region, so the poll does not make it speak every five seconds. The run in progress's events are no live region either: a visually hidden polite `role="status"` says "Run ended" when the run goes. Errors from actions are `role="alert"`.
- **Titles:** `document.title` is "<page> · <team> · Cadre", for example "#12 Fix the build · Platform · Cadre".
- **Keyboard:** everything a pointer can do is reachable by keyboard: the menu, segmented controls (radios), `<details>`, run rows, the cancel confirmation and the list's sort (a button in each header cell; the sorted column's `th` has `aria-sort`). The shortcuts (see [Keyboard shortcuts](#keyboard-shortcuts)) are single keys that never fire while typing, and a dialog gives the focus back to its opener when it closes.
