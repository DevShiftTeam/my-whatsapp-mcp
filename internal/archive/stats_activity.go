package archive

import "time"

type statsInterval struct {
	name string
	step int
}

func (i statsInterval) start(t time.Time) time.Time {
	t = t.UTC()
	switch i.name {
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	case "year":
		year := (t.Year()-1)/i.step*i.step + 1
		return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	default:
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		if i.name == "week" {
			return t.AddDate(0, 0, -(int(t.Weekday())+6)%7)
		}
		return t
	}
}

func (i statsInterval) next(t time.Time) time.Time {
	switch i.name {
	case "month":
		return t.AddDate(0, i.step, 0)
	case "year":
		return t.AddDate(i.step, 0, 0)
	case "week":
		return t.AddDate(0, 0, 7*i.step)
	default:
		return t.AddDate(0, 0, i.step)
	}
}

func statsChooseInterval(first, last time.Time) statsInterval {
	for _, interval := range []statsInterval{{"day", 1}, {"week", 1}, {"month", 1}, {"year", 1}} {
		count := 0
		for at := interval.start(first); !at.After(last) && count <= maxStatsBuckets; at = interval.next(at) {
			count++
		}
		if count <= maxStatsBuckets {
			return interval
		}
	}
	// Keep the response bounded even for valid year-1 to year-9999 filters.
	return statsInterval{"year", (last.Year()-first.Year()+1+maxStatsBuckets-1)/maxStatsBuckets + 1}
}

func (f *readFrame) statsActivity(page *StatsSnapshot, first, last time.Time) error {
	interval := statsChooseInterval(first, last)
	start := interval.start(first)
	page.ActivityFrom, page.ActivityInterval, page.ActivityStep = start.Unix(), interval.name, interval.step
	indexes := make(map[string]int)
	for at := start; !at.After(last); at = interval.next(at) {
		if len(page.Daily) == maxStatsBuckets {
			return ErrLimitExceeded
		}
		date := at.Format("2006-01-02")
		indexes[date] = len(page.Daily)
		page.Daily = append(page.Daily, StatsActivity{Date: date})
		page.ActivityUntil = interval.next(at).Unix()
	}
	// SQLite performs the aggregation. Go materializes at most 90 chart rows;
	// source bodies and an unbounded per-message timeline never leave SQL.
	var expression string
	args := []any{}
	switch interval.name {
	case "day":
		expression = `strftime('%Y-%m-%d',m.original_sent_at/1000.0,'unixepoch')`
	case "week":
		expression = `strftime('%Y-%m-%d',m.original_sent_at/1000.0,'unixepoch','-6 days','weekday 1')`
	case "month":
		expression = `strftime('%Y-%m-01',m.original_sent_at/1000.0,'unixepoch')`
	case "year":
		expression = `printf('%04d-01-01',((CAST(strftime('%Y',m.original_sent_at/1000.0,'unixepoch') AS INTEGER)-1)/?)*?+1)`
		args = append(args, interval.step, interval.step)
	}
	args = append(args, f.args...)
	rows, err := f.tx.QueryContext(f.ctx, `SELECT `+expression+`,COUNT(*) FROM `+authorizedTables+` WHERE `+f.where+` GROUP BY 1 ORDER BY 1 LIMIT 91`, args...)
	if err != nil {
		return readError(f.ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var date string
		var count int64
		if err = rows.Scan(&date, &count); err != nil {
			return readError(f.ctx, err)
		}
		index, exists := indexes[date]
		if !exists {
			return ErrArchiveUnavailable
		}
		page.Daily[index].Messages = count
	}
	return readError(f.ctx, rows.Err())
}
