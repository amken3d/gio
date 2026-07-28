# Provenance

This package is an original, clean-room implementation, written 2026-07-25.

**Replaced code.** Until 2026-07-25 this directory vendored a scanner
derived from the srwiley/scanx adaptation of the Freetype-Go raster
package, dual-licensed under the FreeType License or GPLv2+. That code,
its LICENSE, and its `licenses/` directory were deleted in full before
this implementation was written, and no part of it was consulted during
the rewrite. The maintained Cogent Core fork of the same code was also
checked and found to carry the identical FreeType-Go dual-license header
(verified 2026-07-25), so it was not used either.

**Sources used for this implementation:**

- The published description of the signed area/coverage accumulation
  algorithm ("cl-aa") used by Anti-Grain Geometry:
  http://projects.tuxee.net/cl-vectors/section-the-cl-aa-algorithm
- The call sites in `swrender/render.go` (original code of this fork),
  from which the exported API shape (`Scanner`, `Spanner`, `SpanFunc`)
  was derived for source compatibility.
- Standard fixed-point and polygon-clipping arithmetic.

The only elements retained from the replaced code are the exported
identifier names and signatures required by existing callers, which are
functional interface facts, not copyrightable expression.

**License:** Unlicense OR MIT, same as the rest of this fork
(SPDX-License-Identifier in each file).
