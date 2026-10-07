package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRoutingEventsCursorRetentionAndRollback(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Now()
	e := RoutingEvent{TS: now.Unix(), Policy: "rebalance", Mode: "live", Kind: "switched", Reason: "usage-advantage", Conversation: ConversationReference("private-session"), SourceID: "old", TargetID: "new"}
	if e.Conversation == "private-session" || len(e.Conversation) != 24 {
		t.Fatal("unhashed reference")
	}
	for range 3 {
		if err := AppendRoutingEvent(ctx, db, e); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendRoutingEvent(ctx, tx, e); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	items, err := ListRoutingEvents(ctx, db, 0, 2, now)
	if err != nil || len(items) != 2 || items[0].ID != 3 {
		t.Fatalf("page: %+v %v", items, err)
	}
	older, err := ListRoutingEvents(ctx, db, items[1].ID, 2, now)
	if err != nil || len(older) != 1 || older[0].ID != 1 {
		t.Fatalf("cursor: %+v %v", older, err)
	}
	e.TS = now.Add(-8 * 24 * time.Hour).Unix()
	if err := AppendRoutingEvent(ctx, db, e); err != nil {
		t.Fatal(err)
	}
	items, err = ListRoutingEvents(ctx, db, 0, 200, now)
	if err != nil || len(items) != 3 {
		t.Fatalf("expired visible: %d %v", len(items), err)
	}
	if err := PurgeRoutingEvents(ctx, db, now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM routing_event`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("purge: %d %v", count, err)
	}
	e.Evidence = json.RawMessage(`{"bad":"` + strings.Repeat("x", 2048) + `"}`)
	if err := AppendRoutingEvent(ctx, db, e); err == nil {
		t.Fatal("unbounded evidence accepted")
	}
}

func TestRoutingRetentionBoundedRowCap(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	_, err = db.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<51001)
		INSERT INTO routing_event(ts,policy,mode,kind,reason)
		SELECT ?,'rebalance','live','pending','usage-advantage' FROM n`, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{50001, 50000, 50000} {
		if err := PurgeRoutingEvents(context.Background(), db, now); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM routing_event`).Scan(&count); err != nil || count != want {
			t.Fatalf("bounded retention count=%d want=%d err=%v", count, want, err)
		}
	}
}

func TestRoutingMigrationKeepsUnknownHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO credentials(id,access_token,refresh_token,expires_at,status,created_at) VALUES ('old','fake','fake',4100000000,'active',1);
		INSERT INTO conversations(id,credential_id,created_at,last_seen_at) VALUES ('old','old',1,1);
		INSERT INTO usage_history(credential_id,captured_at,five_hour_pct,seven_day_pct) VALUES ('old',1,0,0);
		ALTER TABLE conversations DROP COLUMN account_bound;
		ALTER TABLE usage_history DROP COLUMN five_hour_observed;
		ALTER TABLE usage_history DROP COLUMN seven_day_observed;`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var fh, sd, affinity int
	if err := db.QueryRow(`SELECT five_hour_observed,seven_day_observed FROM usage_history`).Scan(&fh, &sd); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT account_bound FROM conversations`).Scan(&affinity); err != nil {
		t.Fatal(err)
	}
	if fh != 0 || sd != 0 || affinity != 0 {
		t.Fatal("legacy row was assigned invented evidence")
	}
}
