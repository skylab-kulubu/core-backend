# Memory limit

Core's peak memory comes almost all from media: decoding images and holding
upload bodies. This page is the worst case, what core does to stay under a
container memory limit, and the limit to give core in Dokploy (Swarm).

## What core does

- **Soft limit (`GOMEMLIMIT`).** At startup core reads its container's memory
  limit (cgroup v2 `memory.max`, else v1 `memory.limit_in_bytes`) and sets the
  Go runtime's soft limit to **90 %** of it. Near the soft limit the collector
  runs more often instead of letting the heap grow to twice what is live
  (`GOGC=100`). An explicit `GOMEMLIMIT` env (e.g. `1500MiB`, or `off`) wins;
  without a container limit and without `GOMEMLIMIT` there is none, as
  before. The startup log says which:

  ```
  memory limit: GOMEMLIMIT 1843 MiB, 90% of the container's memory limit (2048 MiB)
  memory limit: no GOMEMLIMIT: the container has no memory limit and GOMEMLIMIT is unset
  ```

  The remaining 10 % is for what the soft limit does not see (page cache
  charged to the container, e.g. multipart temp files; socket buffers) and
  for the collector to catch up. 90 % is also the default of the common
  library for this (`KimMachineGun/automemlimit`); core does it in the
  standard library (`runtime/debug.SetMemoryLimit`, about 60 lines in
  `internal/memlimit`) rather than take a dependency for one file read.
- **Decode budget.** At most `MEDIA_DECODE_SLOTS` (default 2) images are
  decoded at once, process-wide; others wait up to `MEDIA_DECODE_WAIT`
  (default 10 s) and then get `503 media_busy`
  ([media-lifecycle.md, "Decode budget"](media-lifecycle.md#decode-budget)).
- **One copy of each upload.** The upload handlers read the file into one
  buffer of its size (not `io.ReadAll`, which grows step by step and briefly
  holds about 2.25× a 50 MiB file).

A soft limit is not a cap: when more is live than the limit, Go keeps
collecting (its limiter caps the collector at about half the CPU) and memory
still grows; at the container limit the kernel kills core and Swarm starts it
again. The formula below is about what is **live**, so the limit must sit
above it.

## Worst case

```
live ≈ B + S × D + U × R
```

| Term | Value | What |
|---|---|---|
| `B` | ~100 MiB allowance | Core at rest: connection pools, caches, fasthttp's buffers. (Measured in production: far less; this is headroom.) |
| `S` | `MEDIA_DECODE_SLOTS`, default 2 | Images decoded at once. |
| `D` | ≤ 480 MiB | One decode at its largest: the decoded image ≤ 256 MiB (by the header estimate, `maxDecodedImageBytes`), scaling buffers ≤ 128 MiB + 2 × 26 MiB, encoded image and sizes < 40 MiB. |
| `U` | not bounded by core | Single-step uploads in flight at once: being received, waiting for a slot or decoding, images and other files alike (an Answer file is up to 50 MiB). |
| `R` | ≤ ~100 MiB | One upload's body: fasthttp holds the whole request (≤ 51 MiB, `StreamRequestBody` off) and the handler one copy of the file (≤ 50 MiB). A file above 16 MiB is spilled by the multipart parser to a temp file (page cache, not heap); one under it is held once more in the parsed form (≤ 3 × 16 MiB). |

Measured on Go 1.25 (`internal/media`, the service's purpose upload, a
50 MP PNG, 191 MiB live per decode; bodies held as 100 MiB per upload):

| Slots | Uploads at once | Soft limit | Peak Go memory | Peak live heap |
|---|---|---|---|---|
| 1 | 1 | none | 318 MiB | 191 MiB |
| 2 | 2 | none | 628 MiB | 382 MiB |
| 2 | 4 (+100 MiB bodies) | none | 1650 MiB | 848 MiB |
| 2 | 4 (+100 MiB bodies) | 1536 MiB | 1545 MiB | 848 MiB |
| 2 | 8 (+100 MiB bodies) | 1536 MiB | 1623 MiB | 898 MiB |
| 1 | 4 (+100 MiB bodies) | 900 MiB | 910 MiB | 491 MiB |

Without a soft limit the heap reaches about twice what is live; with one it
stays near the limit while what is live fits under it.

## The limit to give core

Per replica (each core task has its own memory and its own decode budget):

| `MEDIA_DECODE_SLOTS` | Container limit | `GOMEMLIMIT` (90 %) | `B + S × D` | Room for 50 MiB uploads at once |
|---|---|---|---|---|
| 2 (default) | **2048 MiB (recommended)** | 1843 MiB | 1060 MiB | ~7 |
| 1 | 1536 MiB | 1382 MiB | 580 MiB | ~8 |
| 1 | 1024 MiB | 921 MiB | 580 MiB | ~3 (too tight) |

**Recommendation: 2048 MiB per core task with the default 2 slots.** The
production node has 15.6 GB and about 8.8 GiB available; two replicas take
4 GiB at most, which they reach only under many large uploads at once. Set
no memory reservation unless Swarm should refuse to place core without it.
Raising `MEDIA_DECODE_SLOTS` needs `D` (480 MiB) more per slot.

The limit is set in the Swarm settings wizard
(`ops/wizards/swarm-settings-wizard.sh`, horizontal-scale ticket 36), not by
core. After setting it, check inside the container:

- the startup log shows the `GOMEMLIMIT … 90% of the container's memory
  limit` line;
- `/v1/metrics` (internal only): `skylab_core_container_memory_limit_bytes`,
  `skylab_core_go_memory_limit_bytes`, `skylab_core_go_memory_bytes` (memory
  the Go runtime holds now) and the decode budget's
  `skylab_media_decode_*`;
- under a test of the largest image (50 MP, 50 MiB) uploaded N at once:
  `/sys/fs/cgroup/memory.peak` stays under the limit and `oom_kill` in
  `/sys/fs/cgroup/memory.events` stays 0.

## What is not bounded

`U`, the uploads in flight, is bounded only by how many people upload at
once (the per-person upload budget limits rate and volume, not concurrency).
Streaming the request body instead of holding it is not a small change in
core: fasthttp's `StreamRequestBody` applies to every route and streams a
body above the body limit instead of refusing it, and the media service
needs the whole file anyway (decoding, ClamAV, the type check read it as
bytes). Ways to bound `U` when it matters: a Traefik `inFlightReq` limit on
the single-step upload routes (`POST /v1/media`,
`POST /v1/users/me/profile-picture`), or moving large files to Direct
upload, whose bytes never pass through core.
