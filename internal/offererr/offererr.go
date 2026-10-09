// Package offererr holds sentinel errors shared between offers and ingest
// without creating an import cycle (offers already imports ingest for the
// stream registry, so ingest cannot import offers back).
package offererr

import "errors"

// ErrNotResolvable is returned when an offer_id does not name a pending
// offer belonging to the calling courier: already resolved, expired,
// unknown, or claimed by someone else. It is safe to expose to clients.
var ErrNotResolvable = errors.New("offer not pending or not yours")
