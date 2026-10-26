# Web UI design system

The web UI is an operations console: minimal, dense and grid-based. Typography, spacing and a small set of semantic colours carry the information, not decoration. Light and dark are designed together. There is one accent, and the status colours are used only for status.

It is React, TypeScript and plain CSS (`web/src/styles.css`), with no component kit, no CSS framework and no new runtime dependency. Nothing is fetched at run time: no web fonts and no icon packages. The UI therefore works in an air-gapped cluster. What the pages do is described in [architecture.md](architecture.md#web-ui-and-api).

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
- on the right, the theme control and the identity.

The team switcher is a menu button. It names the current team: the display name, with the namespace in mono beside it. On `/` it reads "Teams". Its menu holds "All teams", then every team from `GET /teams`. When people log in, each team also shows the caller's role on it.

The switcher gives way before anything else in the bar: a long team name truncates with an ellipsis. The identity keeps its size, and a long user name is cut at its own cap (16rem, 30vw under 640 px, 20vw under 400 px, where the identity also drops its icon and Log out its word, keeping its name); an error from reading `/me` truncates, with the whole of it in its title. Under 640 px the brand's name and the namespace are hidden, the switcher is at most 40vw, and the name form takes the room the bar has left. The bar never scrolls sideways, down to 320 px.

The identity shows:
- with logins: the user's name and a "Log out" ghost button;
- with `--auth=none`: "acting as" the name, with "change";
- while no name is set: the name form, inline.

A skip link to `#main` comes first in the tab order. Each page's `main` has `id="main"`.

#### Team nav

Every team page, `/t/<ns>/...`, has the team nav under the top bar: a `nav` named "Team", 40 px (`--teamnav`), on `--surface`, with a bottom `--line`.
- It holds the team's tabs, each an icon and a label. For now there is one, **Board**, current on `/t/<ns>` only, not on a task's page.
- The current tab has `aria-current="page"`, a 2 px `--accent` underline and `--fg` text; the others are `--fg-muted`.
- A tab can be left out for a role that cannot use it.
- When the tabs do not fit, they scroll inside the nav; the page never scrolls sideways, down to 320 px.
- An unknown team page, `/t/<ns>/<anything else>`, shows Not found under the nav.

The team's URLs and their query parameters are built in `web/src/links.ts` (`teamPath`, `boardLink`, `taskLink`), so a link from one page is one another page reads.

### Pages

- **Teams** (`/`): a card grid of `repeat(auto-fill, minmax(16rem, 1fr))`. Each card shows the display name, the namespace in mono and, with logins, a role badge. The whole card links to the board.
- **Board** (`/t/<ns>`):
  - **Toolbar:**
    - `h1` (the team's name) with the role badge;
    - the live status;
    - the search field (icon, clear button, and "n of m shown" while a query is set);
    - "New task", the one primary button, for members.
  - **Columns:** a horizontal scroller.
    - Under 1024 px it snaps, and each column is 80 vw.
    - From 1024 px there are seven columns of `minmax(15rem, 1fr)`.
    - Each column is a `--surface-2` well with a sticky header: a dot in the phase's hue, the name, and a count pill (`50+` while there is more).
    - The column body scrolls on its own, so headers stay in view. The board fills the viewport below the top bar and the team nav.
    - Terminal columns (Done, Cancelled, Failed) mute their header.
- **Task** (`/t/<ns>/tasks/<name>`):
  - **Header:**
    - a breadcrumb: the team, then `#number`;
    - `h1`: the number in mono, then the title;
    - the phase badge and the priority chip;
    - on the right, "Cancel task" as a danger ghost button, which opens an inline confirmation.
  - **Body:** a grid of `minmax(0, 3fr) minmax(18rem, 1fr)`, stacked under 800 px.
    - The main column holds Description, Thread, the composer and Subtasks.
    - The side column holds Details, the run in progress (while Running) and Runs.
  - `?run=<id>` (any case) opens the page at that run: the run in progress, or an ended run, expanded with its events read. It is scrolled into view once.
- **New task** (`/t/<ns>/new`): one form card, at most 40rem wide.
  - Labels sit above fields, and inputs are 16 px.
  - Assignee and priority are segmented controls.
  - "Create task" is the primary button; "Cancel" is a link back to the board.

## Components

Shared pieces live in `web/src/ui/`.

| Component | What it is |
| --- | --- |
| `Icon` | Inline SVG, 16 px, 1.5 px stroke, `currentColor`. The shapes are Lucide's, and its ISC licence is in the file header. It is `aria-hidden` unless it stands alone with a label. |
| `Button`, `ButtonLink` | Variants: primary, secondary, ghost, danger (outline), danger-ghost, danger-solid, success. `small` gives a 28 px control. An icon-only button has an `aria-label`. |
| `Badge`, `PhaseBadge` | A pill: text in `-fg` on `-soft`. A phase badge has a dot in the phase's hue and the phase's name; a Running one pulses. |
| `PriorityChip` | Urgent and high as an alert icon and the word, in their hue. Low as muted text. Medium, the default, shows nothing on cards and "medium" where the priority is always shown. |
| `RoleBadge` | viewer, member or admin (accent tint). |
| `Pills` | Labels: bordered pills that wrap. |
| `Meta` | A row of an icon and text: an assignee (bot or user), "subtask of", a wait. |
| `Skeleton`, `SkeletonCard`, `Loading` | Placeholder blocks. `Loading` wraps them, is `aria-busy`, and carries a visually hidden "Loading …" for screen readers. |
| `Banner` | An inline error: an alert icon, the message and, where a retry exists, Retry. |
| `Menu`, `MenuNote` | A menu button. It opens a `role="menu"` list of links and focuses the first item. Arrow keys, Home and End move between items, and Escape closes it and returns focus to the button. A click outside, Tab or a choice also closes it. A line that is not a choice (the teams still loading, or failing to load) is a `MenuNote`: a disabled menu item, so assistive technology reads it; it is not highlighted on hover or focus, and choosing it does nothing. |
| `Time` | A `<time>`: relative under a day ("just now", "12 s ago", "5 min ago", "3 h ago"), otherwise the full date. The full date is always in its `title`. |
| `Heading`, `useTitle` | A page's `h1`, which takes focus after a route change, and its document title. |

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
- **Run in progress:** shown while the task is Running. The id in mono links to the run (`?run=<id>`), then the mode, runner and step, and the token totals: "12,345 in · 2,100 out · context 30,120", plus the prompt cache's read and written tokens when there are any. In and out are summed over the events; context is the latest event's, the size of the conversation. Under them the events, as in Runs, read again every 5 s while the tab is visible; "No events yet." until the first. The card goes when the run ends, and Runs reads its list again.
- **Runs:** each run is a row button: a chevron, the mode, runner and reason, and an outcome badge in the outcome's phase hue, with the start and end times under it. The id in mono sits above the button, a link to the run named "Link to run <id>". Expanding a row shows its events in tabular mono: progress lines and token counts, with the prompt cache's when there are any.

## States

| State | How it looks |
| --- | --- |
| Loading | Skeletons shaped like the content: three team cards, seven columns of two cards, the task's header and cards, a runs line. Never the word "Loading…" on screen. |
| Error | An inline banner where the content would be, with Retry where a retry exists. Once the board has loaded, a failed poll keeps its columns and shows the error in the live status: "Could not refresh: <message>". A refused action's reason stays when the role is read again and the action goes, for the composer and for "Cancel task" alike, and it is said once: a refusal for the role is not also shown under the form, and its alert stays the same element as the action it was for goes: the composer's card stays to hold it. |
| Empty | Says what would fill it: "No team yet", with `cadre team init -n <namespace>` in a code block; "Nothing here" in a column; "Nothing in the thread yet."; "No run has ended yet." |
| Not found | A centred card with a link back to Teams. |

## Accessibility

- **Contrast:** text is at 4.5:1 or more in both themes. That includes `--fg-muted` on `--surface`, `--bg` and `--surface-2`, every status `-fg` on `--surface`, `--surface-2` and its `-soft` tint, and text on the accent and on the success and danger buttons. Change a token only after checking it again.
- **Focus:**
  - Every control and card has a 2 px `--ring` `:focus-visible` outline. A card's ring is drawn by its covering link.
  - After a route change, focus moves to the new page's `h1` (`tabindex="-1"`). The first page of a visit keeps the browser's focus.
- **Names:**
  - Icon-only controls (theme, clear search) have an `aria-label`.
  - Decorative icons and skeleton blocks are `aria-hidden`.
  - A Running card's dot is an `img` named "running".
- **Colour alone:** phase and priority never rely on colour only: a phase is a dot and its name, and a priority is an icon and its word. A wait is an icon and the word "Waiting".
- **Structure:** one `h1` per page. Landmarks: `header`, `main` (skip-link target), breadcrumb `nav`, and `region`s for the board's columns and the task's cards.
- **Live regions:** the board has a single polite `role="status"` with `aria-atomic`. Only a change of state reaches it: "Up to date" (visually hidden) or "Could not refresh: <message>". How long ago the board was read ("Updated 12 s ago") is shown beside it, outside the region, so the poll does not make it speak every five seconds. The run in progress's events are no live region either: a visually hidden polite `role="status"` says "Run ended" when the run goes. Errors from actions are `role="alert"`.
- **Titles:** `document.title` is "<page> · <team> · Cadre", for example "#12 Fix the build · Platform · Cadre".
- **Keyboard:** everything a pointer can do is reachable by keyboard: the menu, segmented controls (radios), `<details>`, run rows and the cancel confirmation.
