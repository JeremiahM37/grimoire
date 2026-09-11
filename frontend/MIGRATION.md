# React / TypeScript migration

Status: React/Vite is now the server shell whenever `frontend/dist/index.html`
has been built. `WebDir` remains the icon/static fallback, but the React shell
does not load the retired imperative `app.js`.

Foundation uses React19.3.0 / TypeScript5.9.3 / Vite8.3.0 with strict checking.
`src/types.ts` reflects API response structs, including distinct note-list and
note-detail shapes, tag count `c`, account identity and spaces. `src/api.ts`
preserves account/admin gate distinction and path encoding. Four focused tests
cover gate handling, failed-save non-retry, paths, cancellation and explicit
false values. The React shell must supply `onSessionExpired` after login;
admin gate errors must leave drafts intact.

Implemented: sign-in gate, identity bootstrap, folders and live refresh, note
create/open/edit/autosave/drafts, Markdown headings/lists/tasks/tags/wikilinks,
tag and full-text search, backlinks, daily notes, ask, command palette, graph
rendering, calendar-backed daily-note navigation (including previous/next and
date insertion), task listing/toggling/filtering with note jumps, actionable
history restore and trash restore/purge,
attachment insertion, find/replace-all, heading outline, tag autocomplete,
template save/apply, context duplicate/rename/delete, basic desktop split
editing, and read-only canvas rendering. The administrator slice now has typed
vault initialize/unlock/lock, versioned secret rotation/restore/delete,
masked scan and audit views, scoped grants with revocation and approval
decisions; connector kind-driven setup, edit, pause/resume, sync and optional
imported-note purge; usage windows; trust decisions and stale-note verification;
settings; API-key, space membership, account, and external identity controls.
Admin-gate
failures stay local to those panels and do not reset the open editor or draft.
Graph now filters, zooms, draws visible relationships, and opens a selected
node. Canvas supports creation, a picker for existing boards, background
double-click text cards, text/file card add/edit/delete/drag, edges, wheel and
control zoom, pointer background pan, and persistence of its JSON Canvas
document. The shell registers a versioned service worker generated from the
Vite asset graph; it precaches the React shell, styles, manifest/icon, and the
imperative live-editor engine for offline startup. The mobile drawer has open,
close, and note-selection close behavior.
Plugin settings can list, scaffold, and enable/disable
plugins with the vault-code confirmation warning. Enabled same-origin plugins
activate through an isolated React host, can mount sidebar panels, render
fenced blocks, transform previews, register palette commands, and load assets.

Still incomplete: conflict resolution; dynamic Markdown blocks, transclusion
and plugin renderers; next/previous find, link and slash autocomplete, CM
selection/undo behavior coverage; attachment drag/drop; properties/bookmarks;
version compare; graph controls; plugin slash completion integration; vault
passphrase rotation; account password change and identity removal; mobile
resize/keyboard breadth. Each item needs
behavior tests before the old modules can be removed from the repository.

Keep build output isolated in dist during implementation. Run `npm run build`
before Go/browser suites so the actual replacement is present. Then run strict
build, Go and browser suites and project verify; scaffold tests alone do not
establish migration completion.
