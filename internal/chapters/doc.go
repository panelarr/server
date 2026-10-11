// SPDX-License-Identifier: GPL-3.0-or-later

// Package chapters builds each work's unified chapter list. Every attached
// series' chapters are kept as the source lists them (the per-source
// view); chapters with the same number across sources become one
// canonical chapter with a chosen variant, so gaps in one source are
// filled from another. Update checks refresh the lists on a schedule and
// on demand.
package chapters
