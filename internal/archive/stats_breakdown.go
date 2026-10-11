package archive

import "time"

func (f *readFrame) statsInsights(page *StatsSnapshot) error {
	// Calendar rows remain in SQLite. Subtracting their sequence number from
	// the UTC Julian day groups consecutive days without loading daily history.
	err := f.tx.QueryRowContext(f.ctx, `WITH days AS (
	 SELECT strftime('%Y-%m-%d',m.original_sent_at/1000.0,'unixepoch') AS day,COUNT(*) AS messages
	 FROM `+authorizedTables+` WHERE `+f.where+` GROUP BY 1
	), streaks AS (
	 SELECT julianday(day)-ROW_NUMBER() OVER (ORDER BY day) AS run FROM days
	), runs AS (SELECT COUNT(*) AS length FROM streaks GROUP BY run)
	 SELECT (SELECT COUNT(*) FROM days),COALESCE((SELECT MAX(length) FROM runs),0),
	 COALESCE((SELECT day FROM days ORDER BY messages DESC,day LIMIT 1),''),
	 COALESCE((SELECT MAX(messages) FROM days),0)`, f.args...).Scan(
		&page.Insights.ActiveDays, &page.Insights.LongestStreakDays, &page.Insights.BusiestDate, &page.Insights.BusiestDateMessages)
	return readError(f.ctx, err)
}

func (f *readFrame) statsRhythm(page *StatsSnapshot) error {
	rows, err := f.tx.QueryContext(f.ctx, `SELECT CAST(strftime('%w',m.original_sent_at/1000.0,'unixepoch') AS INTEGER),
	 CAST(strftime('%H',m.original_sent_at/1000.0,'unixepoch') AS INTEGER),COUNT(*)
	 FROM `+authorizedTables+` WHERE `+f.where+` GROUP BY 1,2 ORDER BY 1,2 LIMIT 169`, f.args...)
	if err != nil {
		return readError(f.ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var slot StatsRhythm
		if err = rows.Scan(&slot.Weekday, &slot.Hour, &slot.Messages); err != nil {
			return readError(f.ctx, err)
		}
		if slot.Weekday < 0 || slot.Weekday > 6 || slot.Hour < 0 || slot.Hour > 23 || len(page.Rhythm) == 168 {
			return ErrArchiveUnavailable
		}
		page.Rhythm = append(page.Rhythm, slot)
	}
	return readError(f.ctx, rows.Err())
}

func (f *readFrame) statsBreakdown(page *StatsSnapshot, request StatsRequest) error {
	id, name := "c.id", "c.name"
	page.Breakdown.Total = page.Chats
	if page.Breakdown.Dimension == "people" {
		id, name = "i.id", "i.name"
		if err := f.tx.QueryRowContext(f.ctx, `SELECT COUNT(DISTINCT i.id) FROM `+authorizedTables+` WHERE `+f.where, f.args...).Scan(&page.Breakdown.Total); err != nil {
			return readError(f.ctx, err)
		}
	}
	if page.Breakdown.Total == 0 {
		return nil
	}
	lastPage := (page.Breakdown.Total-1)/int64(page.Breakdown.PageSize) + 1
	requested := int64(request.Page)
	if requested < 1 {
		requested = 1
	}
	if requested > lastPage {
		requested = lastPage
	}
	page.Breakdown.Page = int(requested)
	order := "messages DESC,id"
	switch page.Breakdown.Sort {
	case "name":
		order = "name COLLATE NOCASE,id"
	case "recent":
		order = "last_message_at DESC,id"
	}
	args := append([]any(nil), f.args...)
	args = append(args, page.Breakdown.PageSize, (requested-1)*int64(page.Breakdown.PageSize))
	// Only server-selected expressions enter SQL; user options never do. Both
	// dimensions share the authorized frame and full-contributor expiry guard.
	rows, err := f.tx.QueryContext(f.ctx, `SELECT `+id+` AS id,substr(`+name+`,1,256) AS name,
	 COUNT(*) AS messages,COALESCE(SUM(m.from_me),0),COUNT(DISTINCT m.chat_id),MAX(m.original_sent_at) AS last_message_at
	 FROM `+authorizedTables+` WHERE `+f.where+` GROUP BY `+id+` ORDER BY `+order+` LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return readError(f.ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var row StatsBreakdownRow
		var last int64
		if err = rows.Scan(&row.ID, &row.Name, &row.Messages, &row.Sent, &row.Chats, &last); err != nil {
			return readError(f.ctx, err)
		}
		if len(page.Breakdown.Rows) == page.Breakdown.PageSize {
			return ErrLimitExceeded
		}
		row.Name = boundedOriginal(row.Name, 256)
		row.Received = row.Messages - row.Sent
		row.LastMessageAt = time.UnixMilli(last).Unix()
		page.Breakdown.Rows = append(page.Breakdown.Rows, row)
	}
	return readError(f.ctx, rows.Err())
}
