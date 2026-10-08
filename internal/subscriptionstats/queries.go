package subscriptionstats

import (
	"context"
	"fmt"
	"strings"
)

// Only nonnegative integer counters are meaningful token counts. Missing zero
// counters cannot be distinguished from real zeros; that limitation is disclosed
// rather than claiming every successful response has a complete usage record.
func countExpr(column string) string {
	return "CASE WHEN typeof(r." + column + ")='integer' AND r." + column + ">=0 THEN r." + column + " ELSE 0 END"
}

func sumsSQL(normalizeOpenAI bool) string {
	input, output := countExpr("input_tokens"), countExpr("output_tokens")
	creation, read := countExpr("cache_creation_tokens"), countExpr("cache_read_tokens")
	if normalizeOpenAI {
		input = "CASE WHEN c.provider='custom_openai' THEN (" + input + ")-MIN((" + input + "),(" + read + ")) ELSE (" + input + ") END"
	}
	parts := []string{"COUNT(r.id)"}
	for _, expr := range []string{input, output, creation, read} {
		parts = append(parts, "COALESCE(SUM("+expr+"),0)")
	}
	invalid := []string{}
	for _, column := range []string{"input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens"} {
		invalid = append(invalid, "typeof(r."+column+")!='integer' OR r."+column+"<0")
	}
	parts = append(parts, "COALESCE(SUM(CASE WHEN r.id IS NOT NULL AND ("+strings.Join(invalid, " OR ")+") THEN 1 ELSE 0 END),0)")
	return strings.Join(parts, ",")
}

type aggregate struct {
	requests int64
	tokens   Tokens
	invalid  int64
}

func (a *aggregate) targets() []any {
	return []any{&a.requests, &a.tokens.Input, &a.tokens.Output, &a.tokens.CacheCreation, &a.tokens.CacheRead, &a.invalid}
}

func (b *builder) readActuals(ctx context.Context) error {
	if err := b.readCredentials(ctx); err != nil {
		return err
	}
	var all aggregate
	err := b.conn.QueryRowContext(ctx, `SELECT `+sumsSQL(true)+`
		FROM request_log r LEFT JOIN credentials c ON c.id=r.credential_id
		WHERE r.ts>=? AND r.ts<?`, b.report.From, b.report.To).Scan(all.targets()...)
	if err != nil {
		return err
	}
	if err := all.tokens.total(); err != nil {
		return err
	}
	b.report.Requests, b.report.Tokens = all.requests, all.tokens
	if all.invalid > 0 {
		b.report.Notes = append(b.report.Notes, "Malformed token counters were omitted from recorded totals. Calibration intervals containing malformed counters are not estimated.")
	}
	if err := b.readAccountTotals(ctx); err != nil {
		return err
	}
	if err := b.readBreakdown(ctx, false); err != nil {
		return err
	}
	return b.readBreakdown(ctx, true)
}

func (b *builder) readCredentials(ctx context.Context) error {
	rows, err := b.conn.QueryContext(ctx, `SELECT id,COALESCE(label,''),provider,COALESCE(subscription_type,''),COALESCE(rate_limit_tier,''),created_at
		FROM credentials ORDER BY id LIMIT ?`, maxRows+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for n := 0; rows.Next(); n++ {
		if n == maxRows {
			b.truncate()
			break
		}
		var c credential
		if err := rows.Scan(&c.id, &c.name, &c.provider, &c.plan, &c.tier, &c.createdAt); err != nil {
			return err
		}
		b.account(c, true)
	}
	return rows.Err()
}

const metadataSQL = `COALESCE(c.provider,''),COALESCE(c.subscription_type,''),COALESCE(c.rate_limit_tier,''),c.id IS NOT NULL`

func (b *builder) readAccountTotals(ctx context.Context) error {
	rows, err := b.conn.QueryContext(ctx, `SELECT COALESCE(r.credential_id,''),COALESCE(c.label,''),`+metadataSQL+`,COALESCE(c.created_at,0),`+sumsSQL(true)+`
		FROM request_log r LEFT JOIN credentials c ON c.id=r.credential_id
		WHERE r.ts>=? AND r.ts<? GROUP BY COALESCE(r.credential_id,'')
		ORDER BY COALESCE(r.credential_id,'') LIMIT ?`, b.report.From, b.report.To, maxRows+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for n := 0; rows.Next(); n++ {
		if n == maxRows {
			b.truncate()
			break
		}
		var c credential
		var exists bool
		var v aggregate
		dest := []any{&c.id, &c.name, &c.provider, &c.plan, &c.tier, &exists, &c.createdAt}
		if err := rows.Scan(append(dest, v.targets()...)...); err != nil {
			return err
		}
		if err := v.tokens.total(); err != nil {
			return err
		}
		a := b.account(c, exists)
		a.Requests, a.Tokens = v.requests, v.tokens
		g := b.groups[a.GroupKey]
		g.Requests, err = addInt(g.Requests, v.requests)
		if err != nil {
			return err
		}
		if err := addTokens(&g.Tokens, v.tokens); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Aggregation happens inside SQLite, never by loading 90 days of individual
// requests. Only these two fixed projections can be selected by the caller.
func (b *builder) readBreakdown(ctx context.Context, byModel bool) error {
	bucket := "(r.ts/86400)*86400"
	if byModel {
		bucket = "COALESCE(r.model,'')"
	}
	rows, err := b.conn.QueryContext(ctx, `SELECT `+bucket+` AS bucket,`+metadataSQL+`,`+sumsSQL(true)+`
		FROM request_log r LEFT JOIN credentials c ON c.id=r.credential_id
		WHERE r.ts>=? AND r.ts<?
		GROUP BY bucket,c.provider,c.subscription_type,c.rate_limit_tier,c.id IS NOT NULL
		ORDER BY bucket,c.provider,c.subscription_type,c.rate_limit_tier,c.id IS NOT NULL LIMIT ?`, b.report.From, b.report.To, maxRows+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	type dayKey struct {
		ts    int64
		group string
	}
	type modelKey struct{ model, group string }
	days := make(map[dayKey]*Daily)
	models := make(map[modelKey]*Model)
	for n := 0; rows.Next(); n++ {
		if n == maxRows {
			b.truncate()
			break
		}
		var ts int64
		var model string
		var c credential
		var exists bool
		var v aggregate
		var bucketTarget any = &ts
		if byModel {
			bucketTarget = &model
		}
		dest := []any{bucketTarget, &c.provider, &c.plan, &c.tier, &exists}
		if err := rows.Scan(append(dest, v.targets()...)...); err != nil {
			return err
		}
		if err := v.tokens.total(); err != nil {
			return err
		}
		g := b.group(c, exists)
		var requests *int64
		var tokens *Tokens
		if byModel {
			key := modelKey{model, g.Key}
			m := models[key]
			if m == nil {
				m = &Model{GroupKey: g.Key, Model: model}
				models[key] = m
			}
			requests, tokens = &m.Requests, &m.Tokens
		} else {
			key := dayKey{ts, g.Key}
			d := days[key]
			if d == nil {
				d = &Daily{TS: ts, GroupKey: g.Key}
				days[key] = d
			}
			requests, tokens = &d.Requests, &d.Tokens
		}
		*requests, err = addInt(*requests, v.requests)
		if err != nil {
			return err
		}
		if err := addTokens(tokens, v.tokens); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range days {
		b.report.Daily = append(b.report.Daily, *d)
	}
	for _, m := range models {
		b.report.Models = append(b.report.Models, *m)
	}
	return nil
}

type tokenInterval struct {
	credentialID string
	lo, hi       int64 // Both inclusive, at the log's integer-second resolution.
	value        aggregate
}

// A VALUES table bounds both parameter count and joins. Each disjoint interval
// is an indexed (credential_id,ts) range, not a quota-to-request cross product.
func (b *builder) aggregateIntervals(ctx context.Context, intervals []*tokenInterval) error {
	for offset := 0; offset < len(intervals); offset += intervalBatch {
		end := min(offset+intervalBatch, len(intervals))
		values := make([]string, 0, end-offset)
		args := make([]any, 0, 4*(end-offset))
		for i := offset; i < end; i++ {
			v := intervals[i]
			values = append(values, "(?,?,?,?)")
			args = append(args, i, v.credentialID, v.lo, v.hi)
		}
		query := `WITH bounds(slot,credential_id,lo,hi) AS (VALUES ` + strings.Join(values, ",") + `)
			SELECT b.slot,` + sumsSQL(false) + ` FROM bounds b
			LEFT JOIN request_log r ON r.credential_id=b.credential_id AND r.ts>=b.lo AND r.ts<=b.hi
			GROUP BY b.slot`
		if err := b.readIntervalBatch(ctx, query, args, intervals, offset, end); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) readIntervalBatch(ctx context.Context, query string, args []any, intervals []*tokenInterval, offset, end int) error {
	rows, err := b.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var slot int
		var v aggregate
		if err := rows.Scan(append([]any{&slot}, v.targets()...)...); err != nil {
			return err
		}
		if slot < offset || slot >= end {
			return fmt.Errorf("unexpected interval slot %d", slot)
		}
		if err := v.tokens.total(); err != nil {
			return err
		}
		intervals[slot].value = v
	}
	return rows.Err()
}
