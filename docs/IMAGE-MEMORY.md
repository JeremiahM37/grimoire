# Image memory

Agent memory can hold a picture. This page says what that does and, just as
important, what it does not.

## Recall is by caption; similarity is by pixels

There are two different questions, and they are answered differently.

- **Recall** ("find the picture of the rack wiring") is **by caption only**. The
  caption is the text of the memory fact, and recall, full-text search and the
  vector arm all index that text. A search for a colour, a face, or anything
  visible only in the pixels finds nothing unless the caption says so.
- **Similarity** ("is there already a picture that looks like this one") is
  **by pixels**, with no model and no caption. It is described in
  [Visual similarity](#visual-similarity) below. It answers only with visual
  similarity; it never reports what a picture shows.

A caption can be written by the caller, or, if an operator configures one, by a
vision model ([Automatic captions](#automatic-captions), off by default).

Consequences to plan for:

- The caption is the only thing that makes a picture findable. Write it as you
  would want to search for it: what it shows, where it came from, what it is
  evidence of.
- A picture with no caption is stored, but is **not searchable** and is **never
  recalled**. Its placeholder text (`image (uncaptioned)`) is excluded from every
  recall, search and graph query. The response says `"searchable": false`, and
  the CLI prints `NOT searchable`.
- Caption basis is recorded as `stated` (the caller wrote it), `inferred` (a
  configured vision model wrote it, see below) or `none`. Captions are never
  invented by this code: with no captioner configured, nothing writes one.

## Interfaces

| Surface | How |
|---|---|
| HTTP | `POST /api/memory/image` — JSON `{"data_base64","caption","topic"}` or multipart with a `file` part and `caption`, `topic` fields |
| HTTP | `GET /api/memory/image/{sha}` — the bytes, served only to a caller who can see an entry that refers to them |
| HTTP | `GET /api/memory/image?similar_to=<sha>[&max=<bits>]` — visually similar pictures the caller can see (see below) |
| HTTP | `POST /api/memory/image/similar` — the upload body; returns the similar visible pictures and **stores nothing** |
| MCP | `remember_image` (`data_base64`, `caption`, `topic`) |
| CLI | `grimoire memory image FILE [--caption TEXT] [--topic T]` |
| Recall | each hit with an image carries `"image": {"sha","mime","bytes","url"}` and `"caption_basis"` |

A stored picture answers with `{path, id, text, caption_basis, searchable, image, stripped}`.
`stripped.exif` is `true` when metadata was removed (see below).

## Formats and size

- Accepted: **PNG, JPEG, GIF, WebP**. The format comes from the magic bytes, not
  the file name or the `Content-Type`. Anything else (SVG, HTML, BMP, PDF, …) is
  refused with 400.
- Size cap: **4 MiB** by default. Override with `GRIMOIRE_MEMORY_IMAGE_MAX_BYTES`
  (a malformed or non-positive value falls back to the default, never to "no cap").
  Over the cap is 413.
- A JSON upload is base64, so the largest JSON upload is about 5.6 MB and sits
  inside the server's 8 MiB JSON body limit. Prefer multipart for anything near
  the cap.
- A caption is at most 500 characters.

## Storage

- Content-addressed: the file is `Memory Attachments/<sha256>.<ext>` in the vault,
  where the hash is of the **bytes actually stored** (after metadata removal).
  Identical uploads share one file.
- The memory fact records the hash as `img=<sha256>` and the basis as
  `capb=stated|inferred|none` in its trailer. Both fields are appended after every existing
  field, so bullets written before this feature format byte-identically.
- The directory is flat. The extension is not recorded in the fact; the store
  finds the file by probing the four formats.

### What is removed from the picture

**EXIF metadata is removed from JPEG and PNG** (JPEG `APP1` Exif segments, PNG
`eXIf` chunks). That removes GPS coordinates, camera serial numbers and capture
times in those containers. The whole EXIF block goes, not only the GPS tags,
because a partial rewrite of the embedded TIFF structure is where a stdlib-only
rewriter would corrupt a photo. Nothing is removed silently: the response and the
CLI report it.

**Not removed**, so do not rely on this feature to sanitise these:

- XMP packets in JPEG and PNG (they can also carry location),
- EXIF inside WebP (`EXIF` chunk),
- GIF comment and application extensions,
- anything inside the picture itself: a photographed sign, a face, a screenshot's
  own text.

Pixels are never modified. ICC colour profiles are kept.

## Who can see a picture

A picture is served **only while some non-private memory entry that the caller may
read refers to it**. The check is the same ACL and space rule the rest of the memory
API uses. The consequences:

- A hash alone grants nothing. An unreferenced or hidden picture answers 404 with
  the same body as an absent one, so the endpoint does not confirm which hashes exist.
- Making the note that holds the reference `private` hides the picture from every
  caller. Making it visible again restores it.
- Pictures are written to memory notes, which are commons (see SECURITY-LEAKPROBE
  finding D1). Any member who can read the memory can read its pictures. That is
  the same visibility as the text.

## Deletion

- **Hard forget** of a fact removes the bullet. If it was the last fact that
  referred to its picture, the file is deleted too. Forgetting one of several
  facts that share a picture keeps the file.
- **Soft forget** (retraction) strikes the fact through and keeps it, so the
  picture is kept with it.
- A failed upload removes the bytes it just stored if nothing refers to them.
- The bank cascade (`forget --cascade`) does not touch memory pictures; it works
  on bank models and observations, not on memory bullets.

## Export and import

- **JSONL export** (`GET /api/memory/export?format=jsonl`, `grimoire memory export`):
  each picture's bytes travel once, on the first fact that uses it, as
  `"attachment": {"sha","mime","data"}` with `data` base64. Later facts carry only
  the `image` hash.
- **Markdown export** (`format=markdown`) is one file, so each picture is embedded
  inline as a `data:` URI under its caption. This is a human-readable export and
  there is **no markdown importer**; use JSONL to move memory.
- **JSONL import** (`POST /api/memory/import?from=grimoire`) restores a picture only
  when its bytes hash to the address the fact claims. Bytes that do not match are
  refused. A fact that names a picture with no bytes in the export, and no such
  picture in the store, is refused rather than written as a dangling reference.
  Dry runs check this and write nothing.
- The import body is capped at 16 MiB in total, so an export with many large
  pictures may not fit in one import.

## Visual similarity

`GET /api/memory/image?similar_to=<sha>` and `POST /api/memory/image/similar`
compare **pixel fingerprints**. Standard library only: PNG, JPEG and GIF decode
with `image/*`. WebP has **no signature**, because decoding it needs
`golang.org/x/image`, which is not vendored; a WebP picture is still stored and
still recalled by caption, and a similarity query on it answers 422.

**The fingerprint** (`internal/memimage/signature.go`) is two things, stored in
the index table `image_signatures` keyed by content address:

- a 64-bit **difference hash**: the picture is reduced to a 9x8 grid of mean
  luminances, and each bit records whether a cell is brighter than its right
  neighbour;
- a **4x4x4 RGB histogram** (64 bins), which the hash ignores, so it is what tells
  a recolour apart from a re-crop.

Matches are ranked by Hamming distance on the hash (differing bits, of 64), then
by histogram distance (0 to 1), then by content address, so the order is stable.
The default threshold is **10 bits**; `max=<0..64>` changes it.

**What was measured** (`go test ./internal/memimage -run 'Similarity|Textured' -v`,
generated pictures, so these are idealised; the textured case is the harder one):

| Case | dHash distance (of 64) | Colour distance (0-1) |
|---|---|---|
| identical bytes | 0 | 0 |
| same scene, half size, PNG | 0 | 0.008 |
| same scene, JPEG q85 re-encode | 5 | — |
| same scene, recoloured (tint) | 0 | 0.569 |
| unrelated (seeded noise) | 26 | — |
| textured scene, 2x2 box-resized and JPEG | 5 (unrelated: 29) | — |

Read the table honestly: the hash is blind to a uniform tint that preserves
luminance order, which is why the histogram exists; the threshold of 10 leaves a
wide gap between "same picture, re-encoded or resized" (5) and "unrelated" (26 to
29) on these pictures, and **a gap measured on generated pictures is not a
guarantee for real photographs**. Real photographs with heavy crops, rotation,
flips or large colour edits are not measured here and are not expected to match.

**Who can see a match.** The candidate set is exactly the pictures the caller may
see through `GET /api/memory/image/{sha}`: the same non-private, visibility-free
row filter and the same ACL check. A picture behind a private or sensitive entry
is never compared, never returned, and a `similar_to` probe for it answers 404
like an absent hash. The upload probe writes nothing to memory or to the vault.

**Cost.** The first similarity query decodes each visible picture once and writes
its fingerprint into the index; later queries read the rows. The index is a cache
keyed by an immutable content address, so a reindex does not need to clear it. When a picture is
collected its row is left behind; nothing reads it, because only visible
pictures are ever looked up.

## Automatic captions

**Off by default.** A picture is sent to a model only when an operator configures
one, and only for an upload that carries **no** caption:

| Setting (env) | Meaning |
|---|---|
| `memory_caption_url` (`GRIMOIRE_MEMORY_CAPTION_URL`) | Ollama base URL, or an OpenAI-compatible base ending in `/v1` |
| `memory_caption_model` (`GRIMOIRE_MEMORY_CAPTION_MODEL`) | a vision-capable model; both are required |
| `memory_caption_backend` (`GRIMOIRE_MEMORY_CAPTION_BACKEND`) | `ollama` (default) or `openai` |
| `memory_caption_allow_private` (`GRIMOIRE_MEMORY_CAPTION_ALLOW_PRIVATE`) | `1` to allow loopback and private networks |
| `GRIMOIRE_MEMORY_CAPTION_API_KEY` | bearer token, environment only, never in `settings.json` |

Behaviour, each covered by a test:

- **A human caption always wins.** If the caller supplied a caption the captioner
  is not called and the caption is recorded as `stated`.
- **The write check runs first.** A caller who could not store the picture does
  not cause it to be sent anywhere.
- **The picture sent is the stored bytes**, with EXIF already removed, one request
  per upload, with a 60-second timeout and a 64 KiB cap on the reply.
- **Failure is not an error for the upload.** An unreachable captioner, an error
  status, an empty or malformed reply, or a refused address stores the picture
  uncaptioned (`none`, not searchable), exactly as with no captioner.
- **The address guard.** The connection is checked at dial time against every
  address the name resolves to. Link-local and cloud-metadata addresses are
  refused always; loopback and private networks only with
  `memory_caption_allow_private`. Proxies are not used and redirects are not
  followed. The URL must be http or https with no credentials.
- The reply is cleaned to one line and capped at 500 characters. An inferred
  caption is stored and recalled like any other caption, so **a wrong model
  caption is a wrong memory until someone corrects it**. Basis `inferred` is the
  only marker of that; the response carries `caption_basis: "inferred"`.

The prompt asks for what is visible and tells the model not to guess who is
shown or where it was taken. That is a request, not a guarantee.

## Limits, stated plainly

- Recall is caption-only. A picture is found by a search for words in its caption.
- Similarity is **pixel similarity only**. It cannot tell you what a picture
  shows, and two different scenes that share a bright top half can score as near.
- Similarity is not invariant to rotation, flips, crops or large colour edits, and
  its numbers above come from generated pictures, not a photograph corpus.
- Captions are whatever the caller wrote, or what the configured model wrote.
  No caption from either source, no recall.
- Uncaptioned pictures are not recalled, but they **are** returned by similarity
  queries, because a similarity match does not need a caption to be meaningful.
- Automatic captioning is opt-in and sends the picture to the configured endpoint.
  It is not sent to a hosted service unless you configure one.
- EXIF removal covers JPEG and PNG only, as listed above.
- Pictures are commons, not per-owner.
- Only the four formats are accepted; animated GIF and WebP are stored as the
  files they are, and nothing decodes them.
