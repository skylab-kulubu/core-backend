# pdf-tools

Core's internal PDF service (certificate editor v2): it reads the text of a
background PDF, removes chosen text from it, places its page into a box on a
new page, and renders it to PNG; and it checks the fonts teams upload and
writes them back as WOFF2. Core finds it at `PDF_TOOLS_URL`; with that
unset, the features that need it stay off. Like Gotenberg it is on the
internal network only, with no route from outside.

Python 3.12; PDFium (pypdfium2) reads and renders, pikepdf (qpdf) edits;
OTS (opentype-sanitizer, the checker browsers run on web fonts) and fontTools
check and convert fonts.
The image is `ghcr.io/skylab-kulubu/core-pdf-tools`, built by core-backend's
GHCR workflow with the same tags as core's image (`docs/ci.md`).

## API

Requests are `multipart/form-data` with a `file` part (the PDF) and, where
an endpoint takes them, a `params` part (JSON). `/v1/inspect` and
`/v1/render` also take the PDF as the whole body (`Content-Type:
application/pdf`; `/v1/render?maxPx=`). A body needs a `Content-Length`
(no chunked bodies).

| Endpoint | params | Answer |
|---|---|---|
| `POST /v1/inspect` | – | JSON, below. A PDF of several pages is read (its first page); the others refuse it. |
| `POST /v1/strip-text` | `{"spanIds": ["3", "7.1"]}` | `application/pdf`: the PDF without those texts |
| `POST /v1/place` | `{"pageWidthPt", "pageHeightPt", "rect": {"x", "y", "w", "h"}}` | `application/pdf`: one page of that size, the source page drawn into `rect` |
| `POST /v1/render` | `{"maxPx": 2000}` (16–4000) | `image/png` of the visible page, its long side `maxPx` |
| `POST /v1/font/sanitize` | – | JSON: the font's facts and the checked font as WOFF2, below |
| `GET /healthz` | – | `200 {"status":"ok"}` |

Coordinates are in pt on the **visible page** (the CropBox inside the
MediaBox, turned by `/Rotate`), origin top left, y down: what a viewer
shows.

### `/v1/inspect`

```json
{
  "pages": 1,
  "page": {"widthPt": 842, "heightPt": 595, "rotate": 0},
  "encrypted": false,
  "textCount": 2,
  "truncated": false,
  "objects": {"text": 2, "path": 14, "image": 1, "shading": 1, "form": 0},
  "spans": [
    {
      "id": "12",
      "text": "Katılım Belgesi",
      "bbox": {"x": 220, "y": 166.4, "w": 290.1, "h": 30.2},
      "quad": [[220, 196.6], [510.1, 196.6], [510.1, 166.4], [220, 166.4]],
      "origin": {"x": 220, "y": 195},
      "angle": 0,
      "size": 40,
      "hscale": 1,
      "font": {"name": "Montserrat-Bold", "family": "Montserrat", "weight": 700, "italic": false,
               "serif": false, "mono": false, "script": false, "embedded": true,
               "ascent": 0.968, "descent": -0.251},
      "color": "#1a2659",
      "opacity": 1,
      "renderMode": 0,
      "line": 3, "block": 3, "run": 5, "spaceBefore": false,
      "convertible": true
    }
  ],
  "runs": [{"spanIds": ["12", "13"], "text": "Katılım Belgesi", "line": 3, "block": 3}]
}
```

- A span is one text object of the PDF (one `Tj`, `TJ`, `'` or `"`); some
  producers write a word or a line per object, Chrome and Skia one glyph.
  `id` is its place in the page's object tree (`"7.1"`: object 1 of the
  Form XObject that is page object 7). The same file gives the same ids.
  Objects of spaces only are not listed.
- `text`: NFC (a decomposed ş becomes one character), ligatures spelled out.
- `quad`: the text's box turned with it: bottom left, bottom right, top
  right, top left as the text reads. `bbox` is the quad's bounding box.
  `origin`: where the baseline starts. `angle`: degrees, clockwise as shown
  (CSS `rotate`). `size`: the font size as shown, pt. `hscale`: horizontal
  scaling (1 = none).
- `font.name`: the PDF font's name without a subset prefix (`ABCDEF+`).
  `weight` comes from the name (Thin … Black), else the font descriptor,
  else 400. `ascent` and `descent`: per 1 pt of size.
- `color`: the fill colour as sRGB (CMYK and others converted); the stroke
  colour for stroke-only text. `opacity`: the fill alpha.
- `line`, `block`, `run`, `spaceBefore`: hints. A line is text on one
  baseline next to each other; a block, lines of one style stacked closely;
  a run, one style along a line (what a reader takes for one piece of text);
  `spaceBefore`, a word gap before the span in its run. `runs` lists each
  run's spans and text with those gaps as spaces.
- `convertible: false` comes with a `reason`:

| reason | |
|---|---|
| `unreadable` | U+FFFD or a private-use character, or a font with no known Unicode (Identity encoding, no ToUnicode) |
| `stroke` | outline only (render mode 1) |
| `invisible` | not drawn (mode 3, a scan's OCR layer) |
| `clip` | used as a clip (modes 4–7) |
| `vertical` | vertical writing |
| `skewed` | sheared or mirrored |
| `shared` | in a Form XObject drawn more than once |
| `unmatched` | its operator could not be told apart for removal (below) |

At most 5000 spans are read; `truncated` says there were more.

`objects` counts what the page draws, Form XObjects' contents included:
text objects (spaces-only ones too), paths, images, shadings and Form
XObjects. A page with no text but many paths had its text turned into
outlines (Canva's and Illustrator's outline export); one with an image
and nothing else is a picture. Core says which when a PDF lists no text.

### `/v1/strip-text`

Removes exactly the chosen text-showing operators from the content stream
they are in (the page's or a Form XObject's); every other operator stays as
it was. `'` becomes `T*`, `"` becomes `aw Tw ac Tc T*`. When text follows
on the same line, a `[n] TJ` that moves as far as the removed text did keeps
it in its place. The result is read back: the chosen spans gone, every other
text object in its place (0.05 pt), else `422 strip_incomplete` with
`missing` (span ids not found) and `remaining` (texts still there). Ids that
do not exist or are not convertible: `400 bad_request` with `spanIds`.

How the operators are found: PDFium makes one text object per text-showing
operator that shows at least one character, in content stream order, per
stream. The k-th such operator is the k-th text object; the pairing is
checked operator by operator (font, size, count). A stream where it does not
hold makes its spans `unmatched`.

### `/v1/place`

A new single page, MediaBox = CropBox = `pageWidthPt × pageHeightPt`. The
source's visible page (CropBox and `/Rotate` applied) becomes a Form XObject
drawn with `cm` = `[w/srcW 0 0 h/srcH x pageHeight−y−h]`, so width and height
scale apart (`fill`), and is clipped to the page. Only the page content is
carried: no annotations, form fields or attached files. The box (`rect`) is
the caller's (core's `PlaceBackground`, SPEC §4.2).

### `/v1/render`

The visible page at `maxPx` on its long side, white background, without
annotations or form fields.

### `/v1/font/sanitize`

The font is the `file` part, or the whole body (`Content-Type: font/ttf`,
`font/otf`, `font/woff2`, `font/sfnt` or `application/octet-stream`). Steps,
in order; the first that fails answers:

1. At most 10 MiB (`font_too_large`; a body over 11 MiB is refused before it
   is read). The first bytes: `00 01 00 00` or `true` (TrueType), `OTTO`
   (OpenType/CFF), `wOF2` (WOFF2). `ttcf`, or a WOFF2 of flavor `ttcf`, is
   `font_collection`; anything else (WOFF 1 too) `font_unreadable`.
2. OTS (`ots-sanitize`, in a process of its own) reads it, a WOFF2 unpacked,
   and writes the sanitized sfnt: tables it does not know are dropped, a
   table it cannot read refuses the font (`font_unreadable`, its first error
   in `reason`).
3. fontTools reads the sanitized font: an `fvar` table is `font_variable`;
   OS/2 `fsType` bit 1 (Restricted License embedding) or bit 9 (bitmap
   embedding only: the outlines may not be embedded) is
   `font_embedding_restricted` (with `fsType`).
4. The facts, below.
5. The sanitized font is written as WOFF2 (brotli; the glyf transform and
   quality 11 for small fonts, lighter settings for large ones so that a job
   keeps to its time and memory).

```json
{
  "family": "Deneme Sans",
  "subfamily": "Bold",
  "weight": 700,
  "italic": false,
  "glyphCount": 412,
  "missingTurkish": "ğı",
  "fsType": 0,
  "woff2": "d09GMgABAAAAA…"
}
```

- `family`: name ID 16 (typographic family), else 1; `subfamily`: 17, else
  2. Control characters and runs of spaces become one space.
- `weight`: OS/2 `usWeightClass` (1–9 read as 100–900; none, or out of
  1–1000: 400). `italic`: OS/2 `fsSelection` bit 0 or `head.macStyle`
  bit 1.
- `missingTurkish`: the characters of `ğĞıİşŞçÇöÖüÜ`, `A–Z`, `a–z` and
  `0–9` the font's cmap does not map, in that order (`""`: all there). A
  font missing some is not refused.
- `woff2`: the checked font, base64. This is the only form of the font core
  stores or serves; the upload itself is not used again.

### Errors

RFC 9457 `application/problem+json`, with a `code`:

| code | status | |
|---|---|---|
| `bad_request` | 400 | missing file, bad params, unknown or not convertible span ids |
| `not_found` | 404 | no such path |
| `method_not_allowed` | 405 | a method other than GET and POST (`Allow: GET, POST`) |
| `length_required` | 411 | no `Content-Length` |
| `pdf_too_large` | 413 | over 20 MiB |
| `pdf_unreadable` | 422 | not a PDF that can be read, or one that stopped its worker (over its memory cap, a crash in a library) |
| `pdf_encrypted` | 422 | encrypted or password protected |
| `pdf_pages` | 422 | more than one page (`pages` says how many) |
| `pdf_timeout` | 422 | took longer than 15 s; the worker was stopped |
| `strip_incomplete` | 422 | above |
| `font_too_large` | 413 | a font over 10 MiB |
| `font_unreadable` | 422 | not a TrueType, OpenType or WOFF2 font, refused by OTS, or one that stopped its worker |
| `font_collection` | 422 | a font collection (TTC) |
| `font_variable` | 422 | a variable font (`fvar`) |
| `font_embedding_restricted` | 422 | `fsType` says Restricted License or bitmap-only embedding (`fsType` in the answer) |
| `font_timeout` | 422 | took longer than 10 s; the worker was stopped |
| `busy` | 503 | every worker busy for 10 s, 4 requests already in, or another font being checked for 10 s; `Retry-After: 5` |
| `internal` | 500 | an unexpected error, or no worker could take the job |

A 4xx is the input's: the same request fails again. A 5xx may pass later.

## Limits

- A PDF of at most 20 MiB (the body's `Content-Length` is checked before it
  is read); one page, except for `/v1/inspect`; at most 5000 spans.
- Two worker processes do the PDF and font work; a job runs at most 15 s (a
  font job 10 s, of which OTS has 7), then its worker is killed and
  replaced. A job waits at most 10 s for a worker; at
  most 4 requests are in at once. A worker is replaced after 200 jobs, and
  when it dies; one that cannot be started is tried again on the next job.
- Each worker's data (heap, private writable memory) is capped at 300 MiB
  (`RLIMIT_DATA`): a PDF that inflates to gigabytes fails with
  `pdf_unreadable` (`font_unreadable`) and the next request runs as usual.
  A font of almost 10 MiB is checked within 200 MiB and 10 s on half a CPU
  (the tests measure one).
- A font of at most 10 MiB; OTS's output at most 48 MiB, written to `/tmp`
  (OTS seeks in it) and removed after the job. One font job at a time,
  whatever the workers, so two large fonts never share the 64 MiB `/tmp`: a
  font request waits for the one before it at most 10 s, then is `503 busy`.
  A WOFF2 that inflates far more than a font does is refused by OTS.

Environment: `PORT` (8080), `PDF_TOOLS_WORKERS` (2),
`PDF_TOOLS_TIMEOUT_SECONDS` (15), `PDF_TOOLS_WORKER_MEMORY_MIB` (300; 0 turns
the cap off).

## Running it

PDFs and fonts are untrusted input. Run the container with:

| Setting | Docker | Compose |
|---|---|---|
| read-only root filesystem | `--read-only` | `read_only: true` |
| `/tmp` in memory | `--tmpfs /tmp:size=64m` | `tmpfs: [/tmp:size=64m]` |
| no new privileges | `--security-opt no-new-privileges` | `security_opt: [no-new-privileges:true]` |
| no capabilities | `--cap-drop ALL` | `cap_drop: [ALL]` |
| at most 64 processes | `--pids-limit 64` | `pids_limit: 64` |
| 512 MiB of memory | `--memory 512m` | `mem_limit: 512m` |
| half a CPU | `--cpus 0.5` | `cpus: 0.5` |
| no network out | an internal network shared with core only | |

The image runs as user 10001, which can write only to `/tmp`. It has no
outbound need at all. `deploy/compose.yaml` runs it in the `certs` profile
next to Gotenberg with these settings (`http://pdf-tools:8080` inside the
compose network, `localhost:3010` from the host).

## Development

```sh
cd pdf-tools
python3.12 -m venv .venv
.venv/bin/pip install --require-hashes --no-deps -r requirements.txt -r requirements-test.txt
.venv/bin/python -m pytest -q tests
```

(`requirements*.txt` pin Linux wheels; on macOS install the versions in
`requirements*.in` instead.) The test PDFs are written by the tests
(`tests/fixtures.py`: hand-written content streams and a TrueType font built
with fontTools; `tests/font_fixtures.py`: the fonts); the repository holds no
PDF or font files. CI runs the
tests in the image's `test` stage with the settings above (`docs/ci.md`).

To change a dependency, edit `requirements*.in` and run the `uv pip
compile` line at its top.
