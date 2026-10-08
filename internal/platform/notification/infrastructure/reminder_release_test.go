package infrastructure

import (
	"testing"
	"time"
)

// 每日释放点：北京时间当日 09:00（01:00 UTC）之前创建 → 当日释放；之后 → 次日释放。
func TestNextDailyReleasePoint(t *testing.T) {
	// 北京时间 08:30（00:30 UTC）→ 当日 09:00 北京时间（01:00 UTC）释放。
	before := time.Date(2026, 10, 7, 0, 30, 0, 0, time.UTC)
	if got := nextDailyReleasePoint(before); !got.Equal(time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily release = %v, want same-day 09:00 Beijing (01:00 UTC)", got)
	}
	// 北京时间 11:30（03:30 UTC）→ 已过当日释放点，次日释放。
	after := time.Date(2026, 10, 7, 3, 30, 0, 0, time.UTC)
	if got := nextDailyReleasePoint(after); !got.Equal(time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily release = %v, want next-day 09:00 Beijing (01:00 UTC)", got)
	}
	// 恰在释放点创建：不应立刻可见，落到次日。
	exact := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	if got := nextDailyReleasePoint(exact); !got.Equal(time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily release at exact point = %v, want next day（释放点本身创建不应立刻可见）", got)
	}
}

// 每周释放点：始终落到下一个北京时间周一 09:00（01:00 UTC）。
func TestNextWeeklyReleasePoint(t *testing.T) {
	// 周三（北京时间）→ 下周一释放。
	wednesday := time.Date(2026, 10, 7, 3, 30, 0, 0, time.UTC)
	if got := nextWeeklyReleasePoint(wednesday); !got.Equal(time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekly release = %v, want Monday 2026-10-12 09:00 Beijing (01:00 UTC)", got)
	}
	// 周一北京时间 08:30（00:30 UTC）→ 当日释放。
	mondayBeforeRelease := time.Date(2026, 10, 12, 0, 30, 0, 0, time.UTC)
	if got := nextWeeklyReleasePoint(mondayBeforeRelease); !got.Equal(time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekly release = %v, want same Monday 09:00 Beijing", got)
	}
	// 周一北京时间 10:30（02:30 UTC）→ 下周一释放。
	mondayAfterRelease := time.Date(2026, 10, 12, 2, 30, 0, 0, time.UTC)
	if got := nextWeeklyReleasePoint(mondayAfterRelease); !got.Equal(time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekly release = %v, want next Monday 09:00 Beijing", got)
	}
}
