package main

import "testing"

func TestNormalizeCatalogLineStableAcrossPgDumpReparenthesization(t *testing.T) {
	migration := "constraint core chat_send_permit_check CHECK ((((segment_count >= 1) AND (segment_count <= 32)) AND (segment_index < segment_count)))"
	restored := "constraint core chat_send_permit_check CHECK (((segment_count >= 1) AND (segment_count <= 32) AND (segment_index < segment_count)))"
	if got, want := normalizeCatalogLine(migration), normalizeCatalogLine(restored); got != want {
		t.Fatalf("normalized definitions differ:\n got %q\nwant %q", got, want)
	}
}

func TestNormalizeCatalogLineKeepsLiteralContentAndTokens(t *testing.T) {
	cases := []struct {
		left, right string
		equal       bool
	}{
		{"CHECK (((length(VALUE) >= 1) AND (length(VALUE) <= 128)))", "CHECK ((length(VALUE) >= 1) AND (length(VALUE) <= 128))", true},
		{"CHECK (state = 'a(b)')", "CHECK (state = 'a(b')", false},
		{"CHECK (((x = 'a(b)') AND (y = 1)))", "CHECK ((x = 'a(b)') AND (y = 1))", true},
		{"CHECK (x = 'it''s')", "CHECK (x = 'it''s')", true},
		{"CHECK (x = 'it''s')", "CHECK (x = 'its')", false},
		{"  CHECK\t(x\n= 1)  ", "CHECK ( x = 1 )", true},
	}
	for _, tc := range cases {
		gotLeft, gotRight := normalizeCatalogLine(tc.left), normalizeCatalogLine(tc.right)
		if (gotLeft == gotRight) != tc.equal {
			t.Fatalf("normalize(%q)=%q vs normalize(%q)=%q equal=%t want %t",
				tc.left, gotLeft, tc.right, gotRight, gotLeft == gotRight, tc.equal)
		}
	}
}

func TestSnapshotDifferencesNamesOnlyChangedParts(t *testing.T) {
	left := snapshot{SchemaVersion: 21, SchemaFingerprint: "a", DataFingerprint: "b"}
	right := snapshot{SchemaVersion: 21, SchemaFingerprint: "a", DataFingerprint: "c"}
	got := snapshotDifferences(left, right)
	if len(got) != 1 || got[0] != "data_fingerprint" {
		t.Fatalf("differences = %v, want [data_fingerprint]", got)
	}
	if diffs := snapshotDifferences(left, left); len(diffs) != 0 {
		t.Fatalf("identical snapshots reported differences: %v", diffs)
	}
}
