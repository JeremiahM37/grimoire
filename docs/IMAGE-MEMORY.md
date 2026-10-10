# Image memory

Agent memory can hold a picture. This page says what that does and, just as
important, what it does not.

## Retrieval is by caption only

**An image is found through its caption, never through its pixels.** The caption
is the text of the memory fact, and recall, full-text search and the vector arm
all index that text. Nothing in this feature computes an image embedding, runs a
vision model, or compares picture content. A search for "rack wiring" finds a
picture because someone wrote "rack wiring" in its caption; a search for a colour,
a face, or anything visible only in the pixels finds nothing unless the caption
says so.

Consequences to plan for:

- The caption is the only thing that makes a picture findable. Write it as you
  would want to search for it: what it shows, where it came from, what it is
  evidence of.
- A picture with no caption is stored, but is **not searchable** and is **never
  recalled**. Its placeholder text (`image (uncaptioned)`) is excluded from every
  recall, search and graph query. The response says `"searchable": false`, and
  the CLI prints `NOT searchable`.
- Caption basis is recorded as `stated` (the caller wrote it) or `none`.
  `inferred` (a model wrote the caption from the picture) exists in the data model
  but is **not produced**: the AI layer (`internal/ai`) only sends text prompts to
  its backends and no existing OpenAI-compatible or Anthropic path accepts image
  content, so there is no vision path to use. Captions are never invented.

## Interfaces

| Surface | How |
|---|---|
| HTTP | `POST /api/memory/image` — JSON `{"data_base64","caption","topic"}` or multipart with a `file` part and `caption`, `topic` fields |
| HTTP | `GET /api/memory/image/{sha}` — the bytes, served only to a caller who can see an entry that refers to them |
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
  `capb=stated|none` in its trailer. Both fields are appended after every existing
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

## Limits, stated plainly

- Retrieval is caption-only. There is no pixel search, no visual similarity, and
  no OCR.
- Captions are whatever the caller wrote. No caption, no search.
- No automatic captioning exists. The `inferred` basis is reserved and unused.
- EXIF removal covers JPEG and PNG only, as listed above.
- Pictures are commons, not per-owner.
- Only the four formats are accepted; animated GIF and WebP are stored as the
  files they are, and nothing decodes them.
