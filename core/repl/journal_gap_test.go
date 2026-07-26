package repl

import "testing"

// journalGap mirrors the retention check in handleJournal. A cursor that
// points before the oldest retained entry means the entries in between are
// gone, and the peer must re-bootstrap instead of skipping them.
func journalGap(after, oldest int64) bool { return oldest > after+1 }

func TestJournalGapDetection(t *testing.T) {
	tests := []struct {
		name          string
		after, oldest int64
		want          bool
	}{
		{"empty journal", 0, 0, false},
		{"fresh cursor, nothing pruned", 0, 1, false},
		{"fresh cursor, journal pruned", 0, 32, true},
		{"caught up", 41, 1, false},
		{"cursor just before the oldest retained", 30, 31, false},
		{"cursor two behind the oldest retained", 29, 31, true},
	}
	for _, tt := range tests {
		if got := journalGap(tt.after, tt.oldest); got != tt.want {
			t.Errorf("%s: gap(after=%d, oldest=%d) = %v, want %v",
				tt.name, tt.after, tt.oldest, got, tt.want)
		}
	}
}
