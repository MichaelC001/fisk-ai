//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package runstate

import (
	"fmt"
	"regexp"
)

// idPattern constrains a run id to a safe, single path component. It is also a
// valid NATS subject token, so the same ids carry over to the JetStream backend.
// Operator-chosen names and machine-generated KSUIDs both satisfy it.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// maxIDLen caps a run id so it stays a safe filename and subject token. The
// charset is ASCII, so runes equal bytes.
const maxIDLen = 128

// ValidateID rejects a run id that is not a safe, bounded single path component
// (and a valid NATS subject token). Every backend calls it before an id is used as
// a key or path component, so it is a path-traversal defense as well as a format
// rule and the format cannot drift between backends. It bounds length as well as
// charset: the pattern alone is unbounded, and an oversized id would produce an
// oversized filename.
func ValidateID(id string) error {
	if len(id) > maxIDLen || !idPattern.MatchString(id) {
		return fmt.Errorf("%w: %q (use letters, digits, '-' or '_')", ErrInvalidID, id)
	}

	return nil
}

// PrepareMeta stamps the record format version on a meta record whose Version is
// zero and rejects one carrying a version this build cannot write. Every Store
// calls it on the meta record handed to Create, before that record is appended.
//
// It is here rather than in each caller because the version is the store's fact,
// not the caller's: a caller that left it zero wrote a journal nothing could fold,
// and found out at the resume rather than at the write.
//
// A build writes its own version and no other, so a record already carrying a
// different one is ErrVersion. Reading is where an earlier version will be accepted
// once there is one, converted before it is folded; see Version. Fold refuses every
// version but this one while 1 is the only version there has been.
func PrepareMeta(meta *MetaRecord) error {
	switch meta.Version {
	case 0:
		meta.Version = Version
	case Version:
	default:
		return fmt.Errorf("%w: meta record is version %d, this build writes %d", ErrVersion, meta.Version, Version)
	}

	return nil
}

// CheckAppend is the append contract shared by every Journal: it decides, from the
// last written seq and the seq being appended, whether the append is a duplicate
// to skip or a gap to reject. It is a decision only. The caller still performs the
// write and, crucially, advances its own last-seq only after the record is durably
// stored, so a failed or torn write re-appends the same seq on retry rather than
// silently losing it. Do not fold the last-seq advance into this helper.
//
//   - seq <= lastSeq is an already-recorded duplicate (a crash-retry of the most
//     recent event); skip is true, err is nil, and the append is a no-op.
//   - seq == lastSeq+1 is the next record; skip is false, err is nil.
//   - seq > lastSeq+1 skips ahead of the journal and is ErrSeqGap.
func CheckAppend(lastSeq, seq uint64) (skip bool, err error) {
	if seq <= lastSeq {
		return true, nil
	}
	if seq > lastSeq+1 {
		return false, fmt.Errorf("%w: seq %d skips ahead of %d", ErrSeqGap, seq, lastSeq)
	}

	return false, nil
}
