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

func sumsSQL() string {
	input, output := countExpr("input_tokens"), countExpr("output_tokens")
	creation, read := countExpr("cache_creation_tokens"), countExpr("cache_read_tokens")
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

// Stream the time range once instead of sorting/joining it separately for every
// view. Only bounded aggregate maps survive each row. Limits are independent:
// dropping model detail must not lose account/day counts or global totals.
func (b *builder) readAll(ctx context.Context) error {
	complete, err := b.readCredentials(ctx)
	if err != nil {
		return err
	}
	query := `SELECT COALESCE(r.credential_id,''),r.ts,COALESCE(r.model,''),
		r.input_tokens,r.output_tokens,r.cache_creation_tokens,r.cache_read_tokens`
	join := ""
	var credID, model, rowProvider string
	var ts int64
	var raw [4]any
	targets := []any{&credID, &ts, &model, &raw[0], &raw[1], &raw[2], &raw[3]}
	if !complete {
		// If metadata itself was capped, normalization still needs the real
		// provider for omitted credentials. Do not mislabel them as deleted.
		query += ",COALESCE(c.provider,'')"
		join = " LEFT JOIN credentials c ON c.id=r.credential_id"
		targets = append(targets, &rowProvider)
	}
	rows, err := b.conn.QueryContext(ctx, query+` FROM request_log r`+join+` WHERE r.ts>=? AND r.ts<?`, b.report.From, b.report.To)
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
	unknown := cohort(credential{}, false)
	anyInvalid := false
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rows.Scan(targets...); err != nil {
			return err
		}
		var tokens Tokens
		for i, dst := range []*int64{&tokens.Input, &tokens.Output, &tokens.CacheCreation, &tokens.CacheRead} {
			n, valid := raw[i].(int64)
			if valid && n >= 0 {
				*dst = n
			} else {
				anyInvalid = true
			}
		}
		c, exists := b.credentials[credID]
		if complete {
			rowProvider = c.provider
		}
		// Clamp each request before summing: min(sum(input),sum(cache)) is not
		// equivalent when one upstream response reports cache greater than input.
		if rowProvider == "custom_openai" {
			tokens.Input -= min(tokens.Input, tokens.CacheRead)
		}
		if err := accumulate(&b.report.Requests, &b.report.Tokens, tokens); err != nil {
			return err
		}
		if !complete && !exists {
			continue
		}

		a := b.accounts[credID]
		var g *Group
		if a != nil {
			g = b.groups[a.GroupKey]
		} else {
			g = b.groups[unknown.Key]
			if g == nil && len(b.groups) < maxRows {
				g = b.group(credential{}, false)
			}
			if g == nil {
				b.truncate()
				continue
			}
			if len(b.accounts) < maxRows {
				a = b.account(credential{id: credID}, false)
			} else {
				b.truncate()
			}
		}
		if a != nil {
			if err := accumulate(&a.Requests, &a.Tokens, tokens); err != nil {
				return err
			}
		}
		if err := accumulate(&g.Requests, &g.Tokens, tokens); err != nil {
			return err
		}

		dk := dayKey{(ts / 86400) * 86400, g.Key}
		d := days[dk]
		if d == nil {
			if len(days) < maxRows {
				d = &Daily{TS: dk.ts, GroupKey: g.Key}
				days[dk] = d
			} else {
				b.truncate()
			}
		}
		if d != nil {
			if err := accumulate(&d.Requests, &d.Tokens, tokens); err != nil {
				return err
			}
		}
		mk := modelKey{model, g.Key}
		m := models[mk]
		if m == nil {
			if len(models) < maxRows {
				m = &Model{GroupKey: g.Key, Model: model}
				models[mk] = m
			} else {
				b.truncate()
			}
		}
		if m != nil {
			if err := accumulate(&m.Requests, &m.Tokens, tokens); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if anyInvalid {
		b.report.Notes = append(b.report.Notes, "Malformed token counters were omitted from recorded totals. Calibration intervals containing malformed counters are not estimated.")
	}
	for _, d := range days {
		b.report.Daily = append(b.report.Daily, *d)
	}
	for _, m := range models {
		b.report.Models = append(b.report.Models, *m)
	}
	return nil
}

func accumulate(requests *int64, dst *Tokens, src Tokens) error {
	n, err := addInt(*requests, 1)
	if err != nil {
		return err
	}
	*requests = n
	return addTokens(dst, src)
}

func (b *builder) readCredentials(ctx context.Context) (bool, error) {
	rows, err := b.conn.QueryContext(ctx, `SELECT id,COALESCE(label,''),provider,COALESCE(subscription_type,''),COALESCE(rate_limit_tier,''),created_at
		FROM credentials ORDER BY id LIMIT ?`, maxRows+1)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for n := 0; rows.Next(); n++ {
		if n == maxRows {
			b.truncate()
			return false, nil
		}
		var c credential
		if err := rows.Scan(&c.id, &c.name, &c.provider, &c.plan, &c.tier, &c.createdAt); err != nil {
			return false, err
		}
		b.account(c, true)
	}
	return true, rows.Err()
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
			SELECT b.slot,` + sumsSQL() + ` FROM bounds b
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
