package session

import (
	"testing"
	"time"
)

func TestMerklePrefixMatcherInvalidateAuthInNamespaceBeforePreservesNewerTouchAndOtherNamespace(t *testing.T) {
	matcher := NewMerklePrefixMatcher(time.Hour)
	defer matcher.Clear()

	const (
		authID          = "auth-a"
		targetNamespace = "lcp:v1::claude::model-a::caller"
		otherNamespace  = "lcp:v1::claude::model-b::caller"
	)
	targetTurns := turnsFromTexts("system", "user")
	targetFingerprints, targetMinPrefix := matcher.Prepare(targetTurns)
	initial := matcher.BindFingerprintsWithResult(targetNamespace, targetFingerprints, targetMinPrefix, authID)
	if initial.AccessNumber == 0 {
		t.Fatal("initial bind has no access generation")
	}

	if !matcher.TouchFingerprints(targetNamespace, targetFingerprints, targetMinPrefix, authID) {
		t.Fatal("newer target touch failed")
	}
	otherTurns := turnsFromTexts("system", "different user")
	otherFingerprints, otherMinPrefix := matcher.Prepare(otherTurns)
	matcher.BindFingerprintsWithResult(otherNamespace, otherFingerprints, otherMinPrefix, authID)

	// A stale availability observation must neither evict the newer target touch
	// nor a binding for the same credential in another model namespace.
	matcher.InvalidateAuthInNamespaceBefore(targetNamespace, authID, initial.AccessNumber)

	if match, ok := matcher.MatchFingerprints(targetNamespace, targetFingerprints, targetMinPrefix); !ok || match.AuthID != authID {
		t.Fatalf("newer target binding = %+v, %t; want %q bound", match, ok, authID)
	}
	if match, ok := matcher.MatchFingerprints(otherNamespace, otherFingerprints, otherMinPrefix); !ok || match.AuthID != authID {
		t.Fatalf("other namespace binding = %+v, %t; want %q bound", match, ok, authID)
	}
}
