package retention

import (
	"math/rand/v2"
	"sort"
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func items(times ...string) []Item {
	out := make([]Item, len(times))
	for i, s := range times {
		out[i] = Item{Key: "k/" + s, Time: at(s)}
	}
	return out
}

func keys(ds []Decision) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = strings.TrimPrefix(d.Key, "k/")
	}
	return out
}

func dropKeys(its []Item) []string {
	out := make([]string, len(its))
	for i, d := range its {
		out[i] = strings.TrimPrefix(d.Key, "k/")
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A calendar with gaps: two backups on Oct 22, dailies back to Oct 18,
// weeklies on Oct 15 / 08 / 01, and older monthlies.
var calendar = []string{
	"2026-10-22 03:00", "2026-10-22 01:00", "2026-10-21 03:00", "2026-10-20 03:00",
	"2026-10-19 03:00", "2026-10-18 03:00", // Mon / Sun: ISO week boundary
	"2026-10-15 03:00", "2026-10-08 03:00", "2026-10-01 03:00",
	"2026-09-30 23:59", "2026-09-01 03:00", "2026-08-15 03:00", "2026-07-31 03:00",
}

func TestSelectUnionOfRules(t *testing.T) {
	t.Parallel()
	keep, drop := Select(items(calendar...), Policy{KeepLast: 2, KeepDaily: 3, KeepWeekly: 2, KeepMonthly: 2})

	wantKeep := []string{
		"2026-10-22 03:00", // newest, last, daily, weekly, monthly
		"2026-10-22 01:00", // last
		"2026-10-21 03:00", // daily
		"2026-10-20 03:00", // daily
		"2026-10-18 03:00", // weekly: Sunday Oct 18 is the newest of ISO week Oct 12–18
		"2026-09-30 23:59", // monthly (September)
	}
	if got := keys(keep); !equal(got, wantKeep) {
		t.Errorf("keep = %v\nwant   %v", got, wantKeep)
	}
	wantDrop := []string{"2026-10-19 03:00", "2026-10-15 03:00", "2026-10-08 03:00", "2026-10-01 03:00", "2026-09-01 03:00", "2026-08-15 03:00", "2026-07-31 03:00"}
	if got := dropKeys(drop); !equal(got, wantDrop) {
		t.Errorf("drop = %v\nwant   %v", got, wantDrop)
	}
	if r := strings.Join(keep[0].Reasons, ","); r != "newest,last,daily,weekly,monthly" {
		t.Errorf("newest reasons = %s", r)
	}
	if r := strings.Join(keep[1].Reasons, ","); r != "last" {
		t.Errorf("second reasons = %s", r)
	}
}

func TestSelectNewestAlwaysKept(t *testing.T) {
	t.Parallel()
	keep, drop := Select(items(calendar...), Policy{})
	if len(keep) != 1 || keep[0].Key != "k/2026-10-22 03:00" || keep[0].Reasons[0] != "newest" {
		t.Errorf("zero policy keep = %v", keys(keep))
	}
	if len(drop) != len(calendar)-1 {
		t.Errorf("zero policy drop = %d", len(drop))
	}
}

func TestSelectKeepLastCoversEverything(t *testing.T) {
	t.Parallel()
	keep, drop := Select(items(calendar...), Policy{KeepLast: 100})
	if len(keep) != len(calendar) || len(drop) != 0 {
		t.Errorf("keep=%d drop=%d", len(keep), len(drop))
	}
}

func TestSelectBoundaries(t *testing.T) {
	t.Parallel()
	// Month boundary: one second apart but different months.
	keep, _ := Select(items("2026-10-01 00:00", "2026-09-30 23:59"), Policy{KeepMonthly: 2})
	if got := keys(keep); !equal(got, []string{"2026-10-01 00:00", "2026-09-30 23:59"}) {
		t.Errorf("month boundary keep = %v", got)
	}
	// ISO weeks start on Monday: Sunday Oct 18 and Monday Oct 19 differ.
	keep, _ = Select(items("2026-10-19 00:00", "2026-10-18 23:59"), Policy{KeepWeekly: 2})
	if got := keys(keep); !equal(got, []string{"2026-10-19 00:00", "2026-10-18 23:59"}) {
		t.Errorf("week boundary keep = %v", got)
	}
	// Same day: only the newest is the daily one.
	keep, drop := Select(items("2026-10-19 23:00", "2026-10-19 01:00"), Policy{KeepDaily: 5})
	if len(keep) != 1 || len(drop) != 1 {
		t.Errorf("same day: keep=%v drop=%v", keys(keep), dropKeys(drop))
	}
	// Year boundary in ISO weeks: 2026-12-31 (Thu) and 2027-01-04 (Mon) are different weeks,
	// 2027-01-01 (Fri) is the same ISO week as 2026-12-31.
	keep, _ = Select(items("2027-01-04 00:00", "2027-01-01 00:00", "2026-12-31 00:00"), Policy{KeepWeekly: 2})
	if got := keys(keep); !equal(got, []string{"2027-01-04 00:00", "2027-01-01 00:00"}) {
		t.Errorf("ISO year boundary keep = %v", got)
	}
}

func TestSelectGapsDoNotShrinkRetention(t *testing.T) {
	t.Parallel()
	// Backups stopped for a month; keep-daily 3 still keeps three distinct days.
	keep, _ := Select(items("2026-10-22 03:00", "2026-09-10 03:00", "2026-09-09 03:00", "2026-09-08 03:00", "2026-09-07 03:00"), Policy{KeepDaily: 3})
	if got := keys(keep); !equal(got, []string{"2026-10-22 03:00", "2026-09-10 03:00", "2026-09-09 03:00"}) {
		t.Errorf("keep = %v", got)
	}
}

func TestSelectOrderIndependentAndDeterministic(t *testing.T) {
	t.Parallel()
	base := items(calendar...)
	want, _ := Select(base, Policy{KeepLast: 1, KeepDaily: 2, KeepWeekly: 2, KeepMonthly: 1})
	for i := 0; i < 20; i++ {
		shuffled := make([]Item, len(base))
		copy(shuffled, base)
		rand.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		got, _ := Select(shuffled, Policy{KeepLast: 1, KeepDaily: 2, KeepWeekly: 2, KeepMonthly: 1})
		if !equal(keys(got), keys(want)) {
			t.Fatalf("shuffle %d: %v != %v", i, keys(got), keys(want))
		}
	}
	// Equal timestamps: ordered by key, descending, so the result is stable.
	same := []Item{{Key: "a", Time: at("2026-10-22 03:00")}, {Key: "b", Time: at("2026-10-22 03:00")}}
	keep, drop := Select(same, Policy{})
	if keep[0].Key != "b" || drop[0].Key != "a" {
		t.Errorf("tie break: keep=%s drop=%s", keep[0].Key, drop[0].Key)
	}
}

func TestSelectEmpty(t *testing.T) {
	t.Parallel()
	keep, drop := Select(nil, Policy{KeepLast: 3})
	if keep != nil || drop != nil {
		t.Error("empty input must produce empty output")
	}
	if !(Policy{}).IsZero() || (Policy{KeepMonthly: 1}).IsZero() {
		t.Error("IsZero")
	}
}

func TestSelectDoesNotMutateInput(t *testing.T) {
	t.Parallel()
	in := items("2026-09-01 03:00", "2026-10-22 03:00", "2026-10-01 03:00")
	before := make([]Item, len(in))
	copy(before, in)
	Select(in, Policy{KeepLast: 1})
	if !sort.SliceIsSorted(before, func(i, j int) bool { return i < j }) || in[0] != before[0] || in[2] != before[2] {
		t.Error("input slice was reordered")
	}
}

func TestPad2(t *testing.T) {
	t.Parallel()
	if pad2(3) != "03" || pad2(12) != "12" || pad2(53) != "53" {
		t.Error("pad2")
	}
}
