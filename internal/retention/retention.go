// Package retention decides which backups to keep. It is a pure function of
// the backup timestamps and the policy; it never touches the store.
package retention

import (
	"sort"
	"time"
)

// Policy is the keep-last / daily / weekly / monthly rule set. Zero disables
// a rule. Rules are unioned: a backup survives if any rule keeps it.
type Policy struct {
	KeepLast    int // the newest N backups
	KeepDaily   int // the newest backup of each of the N most recent days that have one
	KeepWeekly  int // ... of each of the N most recent ISO weeks that have one
	KeepMonthly int // ... of each of the N most recent months that have one
}

// IsZero reports whether no rule is configured.
func (p Policy) IsZero() bool {
	return p.KeepLast == 0 && p.KeepDaily == 0 && p.KeepWeekly == 0 && p.KeepMonthly == 0
}

// Item is one backup: its key and creation time.
type Item struct {
	Key  string
	Time time.Time
}

// Decision is a kept item with the rules that kept it.
type Decision struct {
	Item
	Reasons []string
}

// Select partitions items into kept and dropped. The newest item is always
// kept, whatever the policy. Days, weeks and months are evaluated in UTC;
// "the N most recent days" means days that actually have a backup, so a gap
// in the schedule never silently shrinks what is kept.
func Select(items []Item, p Policy) (keep []Decision, drop []Item) {
	sorted := make([]Item, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Time.Equal(sorted[j].Time) {
			return sorted[i].Time.After(sorted[j].Time)
		}
		return sorted[i].Key > sorted[j].Key
	})
	if len(sorted) == 0 {
		return nil, nil
	}

	reasons := make([][]string, len(sorted))
	add := func(i int, r string) { reasons[i] = append(reasons[i], r) }

	add(0, "newest")
	for i := 0; i < len(sorted) && i < p.KeepLast; i++ {
		add(i, "last")
	}
	keepPeriodic(sorted, p.KeepDaily, "daily", func(t time.Time) string { return t.UTC().Format("2006-01-02") }, add)
	keepPeriodic(sorted, p.KeepWeekly, "weekly", func(t time.Time) string {
		y, w := t.UTC().ISOWeek()
		return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006") + "-W" + pad2(w)
	}, add)
	keepPeriodic(sorted, p.KeepMonthly, "monthly", func(t time.Time) string { return t.UTC().Format("2006-01") }, add)

	for i, it := range sorted {
		if len(reasons[i]) > 0 {
			keep = append(keep, Decision{Item: it, Reasons: reasons[i]})
		} else {
			drop = append(drop, it)
		}
	}
	return keep, drop
}

// keepPeriodic keeps the newest item of each of the n most recent periods
// (as named by bucket) that contain an item.
func keepPeriodic(sorted []Item, n int, reason string, bucket func(time.Time) string, add func(int, string)) {
	if n <= 0 {
		return
	}
	seen := map[string]bool{}
	for i, it := range sorted {
		b := bucket(it.Time)
		if seen[b] {
			continue
		}
		if len(seen) >= n {
			return
		}
		seen[b] = true
		add(i, reason)
	}
}

func pad2(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
