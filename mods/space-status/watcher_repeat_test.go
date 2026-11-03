package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fullSnapshot builds a realistic prSnapshot with all fields populated.
func fullSnapshot(head string, comments, reviews, inline []noteRecord) *prSnapshot {
	return &prSnapshot{
		Number: 42,
		Title:  "Fix bug",
		URL:    "https://github.com/o/r/pull/42",
		Fingerprint: snapshotFP{
			Head: head, State: "OPEN", Mergeable: "clean",
			Decision: "APPROVED", Draft: false,
			Checks:   ciDetail{Failed: []string{}, Pending: 0, Passed: 5},
			Comments: comments, Reviews: reviews, Inline: inline,
		},
	}
}

// jsonRoundTrip serializes and deserializes a value through JSON (as the
// state file does) to expose nil-vs-empty differences.
func jsonRoundTrip(t *testing.T, src *prSnapshot) *prSnapshot {
	t.Helper()
	data, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	var dst prSnapshot
	if err := json.Unmarshal(data, &dst); err != nil {
		t.Fatal(err)
	}
	return &dst
}

// slotRoundTrip simulates loading watchSlot state from the JSON file.
func slotRoundTrip(t *testing.T, slot watchSlot) watchSlot {
	t.Helper()
	data, err := jsonText(slot, true)
	if err != nil {
		t.Fatal(err)
	}
	var out watchSlot
	if err := decodeJSON(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. describe: identical snapshots → zero diff
// ---------------------------------------------------------------------------

func TestDescribeIdenticalSnapshotsNoDiff(t *testing.T) {
	snap := fullSnapshot("abc123",
		[]noteRecord{{ID: "1", Author: "bob", At: "2024-01-01T00:00:00Z", Body: "LGTM"}},
		[]noteRecord{{ID: "10", Author: "alice", State: "APPROVED", At: "2024-01-01T00:00:00Z", Body: "Approved"}},
		[]noteRecord{{ID: "20", Author: "carol", At: "2024-01-01T00:00:00Z", Path: "main.go", Body: "fix this"}},
	)
	// A copy with identical data (different pointers).
	dup := fullSnapshot("abc123",
		[]noteRecord{{ID: "1", Author: "bob", At: "2024-01-01T00:00:00Z", Body: "LGTM"}},
		[]noteRecord{{ID: "10", Author: "alice", State: "APPROVED", At: "2024-01-01T00:00:00Z", Body: "Approved"}},
		[]noteRecord{{ID: "20", Author: "carol", At: "2024-01-01T00:00:00Z", Path: "main.go", Body: "fix this"}},
	)
	lines := describe(snap, dup)
	if len(lines) != 0 {
		t.Fatalf("expected 0 diff lines for identical snapshots, got %d: %v", len(lines), lines)
	}
}

// ---------------------------------------------------------------------------
// 2. describe after JSON round-trip → zero diff
//    The state file serializes the stored snapshot. If deserialization
//    introduces nil-vs-empty slice differences, describe would produce
//    false positives and re-notify the agent.
// ---------------------------------------------------------------------------

func TestDescribeAfterJSONRoundTripNoDiff(t *testing.T) {
	original := fullSnapshot("abc123",
		[]noteRecord{{ID: "1", Author: "bob", At: "2024-01-01T00:00:00Z", Body: "LGTM"}},
		[]noteRecord{},
		[]noteRecord{},
	)
	loaded := jsonRoundTrip(t, original)

	// Simulate a fresh fetch that returns identical data.
	fresh := fullSnapshot("abc123",
		[]noteRecord{{ID: "1", Author: "bob", At: "2024-01-01T00:00:00Z", Body: "LGTM"}},
		[]noteRecord{},
		[]noteRecord{},
	)
	lines := describe(loaded, fresh)
	if len(lines) != 0 {
		t.Fatalf("describe(loaded, fresh) = %v; want empty (round-trip should be transparent)", lines)
	}
}

// ---------------------------------------------------------------------------
// 3. ciDetail: nil Failed vs empty Failed
//    reflect.DeepEqual(nil, []string{}) == false.
//    If state deserialization ever produces nil Failed while the live
//    snapshot produces []string{}, describe reports a false CI change.
// ---------------------------------------------------------------------------

func TestCIDetailNilVsEmptyFailed(t *testing.T) {
	// This is the root-cause check: does reflect.DeepEqual distinguish them?
	a := ciDetail{Failed: nil, Pending: 0, Passed: 5}
	b := ciDetail{Failed: []string{}, Pending: 0, Passed: 5}
	if reflect.DeepEqual(a, b) {
		t.Skip("reflect.DeepEqual treats nil and empty slice as equal (not the case in Go)")
	}
	// They ARE different. Now verify that describe exposes this:
	prev := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", Checks: a,
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	curr := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", Checks: b,
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	lines := describe(prev, curr)
	if len(lines) != 0 {
		t.Fatalf("describe produced false CI diff from nil-vs-empty Failed: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// 4. Full watchSlot JSON round-trip → no diff on next snapshot
//    This simulates: delivery succeeds, state written to disk, process
//    restarts, state loaded, next poll fetches identical data.
// ---------------------------------------------------------------------------

func TestWatchSlotRoundTripNoDiff(t *testing.T) {
	snap := fullSnapshot("deadbeef",
		[]noteRecord{{ID: "1", Author: "reviewer", At: "2024-06-01T12:00:00Z", Body: "please fix"}},
		[]noteRecord{{ID: "10", Author: "reviewer", State: "CHANGES_REQUESTED", At: "2024-06-01T12:00:00Z", Body: "needs work"}},
		[]noteRecord{{ID: "20", Author: "reviewer", At: "2024-06-01T12:00:00Z", Path: "foo.go", Body: "typo here"}},
	)
	slot := watchSlot{PR: "42", Snapshot: snap}
	loaded := slotRoundTrip(t, slot)

	fresh := fullSnapshot("deadbeef",
		[]noteRecord{{ID: "1", Author: "reviewer", At: "2024-06-01T12:00:00Z", Body: "please fix"}},
		[]noteRecord{{ID: "10", Author: "reviewer", State: "CHANGES_REQUESTED", At: "2024-06-01T12:00:00Z", Body: "needs work"}},
		[]noteRecord{{ID: "20", Author: "reviewer", At: "2024-06-01T12:00:00Z", Path: "foo.go", Body: "typo here"}},
	)
	lines := describe(loaded.Snapshot, fresh)
	if len(lines) != 0 {
		t.Fatalf("false diff after slot round-trip: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// 5. State map round-trip (as loadState/saveState does)
// ---------------------------------------------------------------------------

func TestStateMapRoundTripNoDiff(t *testing.T) {
	snap := fullSnapshot("abc",
		[]noteRecord{},
		[]noteRecord{},
		[]noteRecord{},
	)
	state := map[string]watchSlot{
		"terminal-1": {PR: "42", Snapshot: snap},
	}
	data, err := jsonText(state, true)
	if err != nil {
		t.Fatal(err)
	}
	var loaded map[string]watchSlot
	if err := decodeJSON(append(data, '\n'), &loaded); err != nil {
		t.Fatal(err)
	}

	fresh := fullSnapshot("abc",
		[]noteRecord{},
		[]noteRecord{},
		[]noteRecord{},
	)
	lines := describe(loaded["terminal-1"].Snapshot, fresh)
	if len(lines) != 0 {
		t.Fatalf("false diff after state map round-trip: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// 6. Multi-poll simulation: deliver, then identical data → no repeat
// ---------------------------------------------------------------------------

func TestMultiPollNoRepeatAfterDelivery(t *testing.T) {
	agents := []watchAgent{{Address: "agent-1"}}
	state := map[string]watchSlot{}

	comment := noteRecord{ID: "1", Author: "bob", At: "2024-01-01T00:00:00Z", Body: "Fix this"}

	// --- Poll 1: baseline ---
	snap1 := fullSnapshot("aaa", []noteRecord{}, []noteRecord{}, []noteRecord{})
	_, hasBaseline := groupUpdateLines(agents, state, snap1)
	if hasBaseline {
		t.Fatal("poll 1: expected no baseline")
	}
	state["agent-1"] = watchSlot{PR: "42", Snapshot: snap1}

	// --- Poll 2: new comment → should report ---
	snap2 := fullSnapshot("aaa", []noteRecord{comment}, []noteRecord{}, []noteRecord{})
	lines, hasBaseline := groupUpdateLines(agents, state, snap2)
	if !hasBaseline || len(lines) != 1 {
		t.Fatalf("poll 2: expected 1 line, got %d: %v (baseline=%v)", len(lines), lines, hasBaseline)
	}
	// Simulate successful delivery: update state.
	state["agent-1"] = watchSlot{PR: "42", Snapshot: snap2}

	// --- Poll 3: identical data → no diff ---
	snap3 := fullSnapshot("aaa", []noteRecord{comment}, []noteRecord{}, []noteRecord{})
	lines, hasBaseline = groupUpdateLines(agents, state, snap3)
	if !hasBaseline {
		t.Fatal("poll 3: lost baseline")
	}
	if len(lines) != 0 {
		t.Fatalf("poll 3: REPEAT NOTIFICATION: %v", lines)
	}

	// --- Poll 4: still identical → still no diff ---
	snap4 := fullSnapshot("aaa", []noteRecord{comment}, []noteRecord{}, []noteRecord{})
	lines, _ = groupUpdateLines(agents, state, snap4)
	if len(lines) != 0 {
		t.Fatalf("poll 4: REPEAT NOTIFICATION: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// 7. Multi-poll with pending (busy agent) → deliver on retry, then quiet
// ---------------------------------------------------------------------------

func TestMultiPollPendingThenDelivery(t *testing.T) {
	agents := []watchAgent{{Address: "agent-1"}}
	state := map[string]watchSlot{}

	comment := noteRecord{ID: "1", Author: "bob", At: "2024-01-01T00:00:00Z", Body: "Fix this"}

	// Baseline.
	snap0 := fullSnapshot("aaa", []noteRecord{}, []noteRecord{}, []noteRecord{})
	state["agent-1"] = watchSlot{PR: "42", Snapshot: snap0}

	// --- Poll 1: new comment, agent busy ---
	snap1 := fullSnapshot("aaa", []noteRecord{comment}, []noteRecord{}, []noteRecord{})
	lines, _ := groupUpdateLines(agents, state, snap1)
	if len(lines) != 1 {
		t.Fatalf("poll 1: expected 1 line, got %d", len(lines))
	}
	// Delivery failed: keep old snapshot, store pending.
	state["agent-1"] = watchSlot{PR: "42", Snapshot: snap0, Pending: lines}

	// --- Poll 2: same data, agent busy ---
	snap2 := fullSnapshot("aaa", []noteRecord{comment}, []noteRecord{}, []noteRecord{})
	lines, _ = groupUpdateLines(agents, state, snap2)
	if len(lines) != 1 {
		t.Fatalf("poll 2: expected 1 line (retry), got %d", len(lines))
	}
	// Still failed.
	state["agent-1"] = watchSlot{PR: "42", Snapshot: snap0, Pending: lines}

	// --- Poll 3: agent free, delivery succeeds ---
	snap3 := fullSnapshot("aaa", []noteRecord{comment}, []noteRecord{}, []noteRecord{})
	lines, _ = groupUpdateLines(agents, state, snap3)
	if len(lines) != 1 {
		t.Fatalf("poll 3: expected 1 line for delivery, got %d", len(lines))
	}
	state["agent-1"] = watchSlot{PR: "42", Snapshot: snap3}

	// --- Poll 4: nothing changed → silence ---
	snap4 := fullSnapshot("aaa", []noteRecord{comment}, []noteRecord{}, []noteRecord{})
	lines, _ = groupUpdateLines(agents, state, snap4)
	if len(lines) != 0 {
		t.Fatalf("poll 4: REPEAT NOTIFICATION after pending delivery: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// 8. Pending delivery → new event stacks → deliver combined, then quiet
// ---------------------------------------------------------------------------

func TestPendingPlusNewEventNoDuplicate(t *testing.T) {
	agents := []watchAgent{{Address: "agent-1"}}

	c1 := noteRecord{ID: "1", Author: "bob", At: "2024-01-01T00:00:00Z", Body: "Fix this"}
	c2 := noteRecord{ID: "2", Author: "alice", At: "2024-01-02T00:00:00Z", Body: "Also fix that"}

	snap0 := fullSnapshot("aaa", []noteRecord{}, []noteRecord{}, []noteRecord{})
	state := map[string]watchSlot{"agent-1": {PR: "42", Snapshot: snap0}}

	// Poll 1: c1 appears, agent busy.
	snap1 := fullSnapshot("aaa", []noteRecord{c1}, []noteRecord{}, []noteRecord{})
	lines, _ := groupUpdateLines(agents, state, snap1)
	if len(lines) != 1 {
		t.Fatalf("poll 1: expected 1 line, got %d", len(lines))
	}
	state["agent-1"] = watchSlot{PR: "42", Snapshot: snap0, Pending: lines}

	// Poll 2: c2 added, agent still busy. describe sees both c1 and c2 as new.
	snap2 := fullSnapshot("aaa", []noteRecord{c1, c2}, []noteRecord{}, []noteRecord{})
	lines, _ = groupUpdateLines(agents, state, snap2)
	if len(lines) != 2 {
		t.Fatalf("poll 2: expected 2 lines (c1 + c2), got %d: %v", len(lines), lines)
	}
	state["agent-1"] = watchSlot{PR: "42", Snapshot: snap0, Pending: lines}

	// Poll 3: agent free, deliver all.
	snap3 := fullSnapshot("aaa", []noteRecord{c1, c2}, []noteRecord{}, []noteRecord{})
	lines, _ = groupUpdateLines(agents, state, snap3)
	if len(lines) != 2 {
		t.Fatalf("poll 3: expected 2 lines, got %d", len(lines))
	}
	state["agent-1"] = watchSlot{PR: "42", Snapshot: snap3}

	// Poll 4: quiet.
	snap4 := fullSnapshot("aaa", []noteRecord{c1, c2}, []noteRecord{}, []noteRecord{})
	lines, _ = groupUpdateLines(agents, state, snap4)
	if len(lines) != 0 {
		t.Fatalf("poll 4: REPEAT NOTIFICATION: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// 9. CI only: pending→passed should notify once
// ---------------------------------------------------------------------------

func TestCITransitionNotifiesOnce(t *testing.T) {
	agents := []watchAgent{{Address: "agent-1"}}

	mkSnap := func(pending, passed int, failed []string) *prSnapshot {
		return &prSnapshot{
			Number: 42, Title: "Fix", URL: "url",
			Fingerprint: snapshotFP{
				Head: "aaa", State: "OPEN", Mergeable: "clean",
				Checks:   ciDetail{Failed: failed, Pending: pending, Passed: passed},
				Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
			},
		}
	}

	state := map[string]watchSlot{
		"agent-1": {PR: "42", Snapshot: mkSnap(2, 3, []string{})},
	}

	// Poll: checks finish.
	current := mkSnap(0, 5, []string{})
	lines, _ := groupUpdateLines(agents, state, current)
	if len(lines) != 1 {
		t.Fatalf("expected 1 CI line, got %d: %v", len(lines), lines)
	}
	state["agent-1"] = watchSlot{PR: "42", Snapshot: current}

	// Next poll: same CI → no repeat.
	next := mkSnap(0, 5, []string{})
	lines, _ = groupUpdateLines(agents, state, next)
	if len(lines) != 0 {
		t.Fatalf("REPEAT CI notification: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// 10. CI nil-vs-empty Failed after JSON round-trip
//     This is the specific edge-case that can cause phantom CI diffs.
// ---------------------------------------------------------------------------

func TestCIRoundTripNilVsEmpty(t *testing.T) {
	snap := &prSnapshot{
		Number: 42, Title: "Fix", URL: "url",
		Fingerprint: snapshotFP{
			Head: "aaa", State: "OPEN", Mergeable: "clean",
			Checks:   ciDetail{Failed: []string{}, Pending: 0, Passed: 5},
			Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
		},
	}

	loaded := jsonRoundTrip(t, snap)

	// After JSON round-trip, does Failed remain non-nil?
	if loaded.Fingerprint.Checks.Failed == nil {
		t.Log("WARNING: JSON round-trip turned []string{} into nil for ciDetail.Failed")
	}

	// Regardless of nil/empty, describe should NOT report a false diff.
	fresh := &prSnapshot{
		Number: 42, Title: "Fix", URL: "url",
		Fingerprint: snapshotFP{
			Head: "aaa", State: "OPEN", Mergeable: "clean",
			Checks:   ciDetail{Failed: []string{}, Pending: 0, Passed: 5},
			Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
		},
	}
	lines := describe(loaded, fresh)
	if len(lines) != 0 {
		t.Fatalf("REPEAT NOTIFICATION from nil-vs-empty Failed after JSON round-trip: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// 11. Comments/Reviews/Inline nil-vs-empty after JSON round-trip
// ---------------------------------------------------------------------------

func TestNoteSlicesRoundTripNoDiff(t *testing.T) {
	// Build a snapshot where all note slices are explicitly empty (non-nil).
	snap := fullSnapshot("aaa", []noteRecord{}, []noteRecord{}, []noteRecord{})
	loaded := jsonRoundTrip(t, snap)

	// Now build a fresh snapshot with non-nil empty slices.
	fresh := fullSnapshot("aaa", []noteRecord{}, []noteRecord{}, []noteRecord{})
	lines := describe(loaded, fresh)
	if len(lines) != 0 {
		t.Fatalf("false diff from note-slice nil-vs-empty after round-trip: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// 12. Two agents, same PR: delivery to one updates both, no repeat
// ---------------------------------------------------------------------------

func TestTwoAgentsSamePRNoRepeat(t *testing.T) {
	agents := []watchAgent{{Address: "a1"}, {Address: "a2"}}
	snap0 := fullSnapshot("aaa", []noteRecord{}, []noteRecord{}, []noteRecord{})
	state := map[string]watchSlot{
		"a1": {PR: "42", Snapshot: snap0},
		"a2": {PR: "42", Snapshot: snap0},
	}

	comment := noteRecord{ID: "1", Author: "bob", At: "2024-01-01T00:00:00Z", Body: "Fix"}
	snap1 := fullSnapshot("aaa", []noteRecord{comment}, []noteRecord{}, []noteRecord{})
	lines, _ := groupUpdateLines(agents, state, snap1)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d: %v", len(lines), lines)
	}
	// Delivery to a1 succeeds. Both agents updated.
	for _, a := range agents {
		state[a.Address] = watchSlot{PR: "42", Snapshot: snap1}
	}

	// Next poll: no repeat.
	snap2 := fullSnapshot("aaa", []noteRecord{comment}, []noteRecord{}, []noteRecord{})
	lines, _ = groupUpdateLines(agents, state, snap2)
	if len(lines) != 0 {
		t.Fatalf("REPEAT NOTIFICATION for two-agent group: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// 13. Mergeable unknown oscillation should not re-notify
// ---------------------------------------------------------------------------

func TestMergeableUnknownOscillationNoDiff(t *testing.T) {
	agents := []watchAgent{{Address: "a1"}}
	// State has known mergeable.
	state := map[string]watchSlot{
		"a1": {Snapshot: &prSnapshot{
			Number: 42, Fingerprint: snapshotFP{
				Head: "aaa", Mergeable: "clean",
				Checks:   ciDetail{Failed: []string{}},
				Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
			},
		}},
	}

	// API returns "unknown" repeatedly — should never notify.
	for i := 0; i < 5; i++ {
		current := &prSnapshot{
			Number: 42, Fingerprint: snapshotFP{
				Head: "aaa", Mergeable: "unknown",
				Checks:   ciDetail{Failed: []string{}},
				Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
			},
		}
		preserveKnownMergeable(agents, state, current)
		lines, _ := groupUpdateLines(agents, state, current)
		if len(lines) != 0 {
			t.Fatalf("poll %d: false mergeable notification: %v", i, lines)
		}
		state["a1"] = watchSlot{Snapshot: current}
	}
}

// ---------------------------------------------------------------------------
// 14. Snapshot with CI Failed: nil from a manually constructed or
//     zero-value ciDetail → should still compare safely.
// ---------------------------------------------------------------------------

func TestDescribeCIZeroValueVsPopulated(t *testing.T) {
	prev := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa",
		// Checks is zero-value: Failed is nil
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	curr := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa",
		Checks:   ciDetail{Failed: []string{}, Pending: 0, Passed: 0},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	lines := describe(prev, curr)
	if len(lines) != 0 {
		t.Fatalf("false CI diff from zero-value vs empty ciDetail: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// 15. Full end-to-end: state saved → process "restarts" (load from JSON) →
//     next poll with identical data → no notification.
// ---------------------------------------------------------------------------

func TestProcessRestartNoDuplicateNotification(t *testing.T) {
	agents := []watchAgent{{Address: "terminal-1"}}
	comment := noteRecord{ID: "5", Author: "reviewer", At: "2024-03-15T10:00:00Z", Body: "please rebase"}

	// State from before "restart": agent has seen the comment.
	snap := fullSnapshot("cafebabe",
		[]noteRecord{comment},
		[]noteRecord{{ID: "100", Author: "maintainer", State: "CHANGES_REQUESTED", At: "2024-03-15T10:00:00Z", Body: "needs rebase"}},
		[]noteRecord{{ID: "200", Author: "maintainer", At: "2024-03-15T10:00:00Z", Path: "file.go", Body: "unused import"}},
	)
	state := map[string]watchSlot{
		"terminal-1": {PR: "42", Snapshot: snap},
	}

	// Simulate save + load (JSON round-trip of the full state map).
	data, err := jsonText(state, true)
	if err != nil {
		t.Fatal(err)
	}
	var loaded map[string]watchSlot
	if err := decodeJSON(append(data, '\n'), &loaded); err != nil {
		t.Fatal(err)
	}

	// "Next poll" after restart: identical data from GitHub.
	fresh := fullSnapshot("cafebabe",
		[]noteRecord{comment},
		[]noteRecord{{ID: "100", Author: "maintainer", State: "CHANGES_REQUESTED", At: "2024-03-15T10:00:00Z", Body: "needs rebase"}},
		[]noteRecord{{ID: "200", Author: "maintainer", At: "2024-03-15T10:00:00Z", Path: "file.go", Body: "unused import"}},
	)

	lines, hasBaseline := groupUpdateLines(agents, loaded, fresh)
	if !hasBaseline {
		t.Fatal("lost baseline after restart")
	}
	if len(lines) != 0 {
		t.Fatalf("REPEAT NOTIFICATION after process restart: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// 16. Pending lines should NOT fire when describe already finds new changes.
//     (Regression: pending could stack with new lines, doubling the message.)
// ---------------------------------------------------------------------------

func TestPendingIgnoredWhenNewDiffExists(t *testing.T) {
	agents := []watchAgent{{Address: "a1"}}

	c1 := noteRecord{ID: "1", Author: "bob", At: "2024-01-01T00:00:00Z", Body: "Fix this"}
	c2 := noteRecord{ID: "2", Author: "alice", At: "2024-01-02T00:00:00Z", Body: "Also fix that"}

	snap0 := fullSnapshot("aaa", []noteRecord{}, []noteRecord{}, []noteRecord{})
	state := map[string]watchSlot{
		"a1": {PR: "42", Snapshot: snap0, Pending: []string{"- Comment from bob: Fix this"}},
	}

	// New poll has both c1 (pending) and c2 (new).
	snap1 := fullSnapshot("aaa", []noteRecord{c1, c2}, []noteRecord{}, []noteRecord{})
	lines, _ := groupUpdateLines(agents, state, snap1)

	// describe(snap0, snap1) finds c1 AND c2 as new. Pending should be ignored.
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines from describe (not pending), got %d: %v", len(lines), lines)
	}
}

// ---------------------------------------------------------------------------
// 17. Empty pending falls back correctly.
// ---------------------------------------------------------------------------

func TestEmptyPendingDoesNotNotify(t *testing.T) {
	agents := []watchAgent{{Address: "a1"}}
	snap := fullSnapshot("aaa", []noteRecord{}, []noteRecord{}, []noteRecord{})
	state := map[string]watchSlot{
		"a1": {PR: "42", Snapshot: snap, Pending: []string{}},
	}

	fresh := fullSnapshot("aaa", []noteRecord{}, []noteRecord{}, []noteRecord{})
	lines, _ := groupUpdateLines(agents, state, fresh)
	if len(lines) != 0 {
		t.Fatalf("unexpected notification from empty pending: %v", lines)
	}
}
