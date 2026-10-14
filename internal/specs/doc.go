// Duplicate-upload and update policy.
//
// Identity: an uploaded file is identified by the SHA-256 of its exact bytes
// (content hash). A Spec record has a stable ID that is separate from content
// versions and processing runs. Filename alone is never identity: two uploads
// with the same name but different bytes are different versions.
//
// Matching an upload against existing records (in order):
//
//  1. Identical content (same SHA-256) with a Completed run whose outputs are
//     still present on disk: no regeneration. The existing record is returned
//     with deduped=true.
//  2. Identical content already Queued/Processing: no duplicate job. The
//     existing record is returned with a 409-style conflict signal so callers
//     can avoid launching a second job for the same spec version.
//  3. Identical content whose last run Failed: the existing record is
//     returned; the caller may retry explicitly.
//  4. Different content but the same logical spec name (spec.name field):
//     the bytes are stored as a new version under the same Spec ID, the
//     previous successful output is kept, Stale is set to true, and a new run
//     is scheduled. Successful runs promote via a staging directory with an
//     atomic rename, so a failed update never destroys the last good output.
//  5. Different content with a new (or empty) name: a new Spec ID is
//     registered and scheduled.
//
// Retries are idempotent: retrying a Queued/Processing version is a no-op
// conflict; retrying a terminal version schedules exactly one new run for the
// current content hash. Concurrent uploads of identical bytes for the same
// spec are collapsed by a per-spec mutex plus a hash check inside the critical
// section.
package specs
