package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/creds"
	"github.com/p4u/claude-proxy/internal/store"
)

// Elective session-rebalance contract (the implementation lives in the
// handler; these tests only pin its observable behaviour):
//
//   - Handler.RebalanceSessions defaults to true; setting it to false opts a
//     deployment out entirely — no notice headers, no pin movement.
//   - A long-lived Anthropic pin (bound_at >= 1h ago, falling back to
//     created_at when bound_at is 0) on a heavily used account qualifies for a
//     rebalance when a sibling account of the same base URL is in much better
//     shape: two usage samples per account at least 5 minutes apart, both
//     windows' resets still in the future, both credentials active.
//   - The qualifying message is still served by the pinned account, and its
//     200 response carries X-Router-Rebalance: pending plus a human-readable
//     X-Router-Message that names no credential ID, label or token. The next
//     eligible message after that request completes is served by the other
//     account and carries X-Router-Rebalance: switched. A third message sticks
//     (1h cooldown).
//   - count_tokens shares the conversation binding but must neither initiate
//     nor execute the switch. Error responses (429/500) never acknowledge the
//     handshake. X-Router-* headers sent by the upstream are discarded; only
//     the router's own values reach the client. Request and response bodies
//     are relayed byte-identically in both directions.

func BenchmarkPortableRebalanceRequest(b *testing.B) {
	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"` + strings.Repeat("word ", 200000) + `"}]}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if !portableRebalanceRequest(body) {
			b.Fatal("ordinary conversation rejected")
		}
	}
}

func TestPortableRebalanceRequest(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"ordinary", rebalanceReqBody, true},
		{"client tools", `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"Read","input":{"path":"test.go"}}]}]}`, true},
		{"quoted marker", `{"messages":[{"role":"user","content":"what is file_id or container?"}]}`, true},
		{"file reference", `{"messages":[{"content":[{"source":{"type":"file","file_id":"file_test"}}]}]}`, false},
		{"escaped file key", `{"` + string(rune(92)) + `u0066ile_id":"file_test"}`, false},
		{"escaped tool type", `{"tools":[{"type":"` + string(rune(92)) + `u0063ode_execution_20250825"}]}`, false},
		{"container string", `{"container":"container_test"}`, false},
		{"container object", `{"container":{"id":"container_test"}}`, false},
		{"empty container", `{"container":null}`, true},
		{"server tool history", `{"messages":[{"content":[{"type":"server_tool_use","id":"srvtool_test"}]}]}`, false},
		{"execution tool", `{"tools":[{"type":"code_execution_20250825"}]}`, false},
		{"invalid", `{bad`, false},
		{"array", `[]`, false},
		{"null", `null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := portableRebalanceRequest([]byte(tc.body)); got != tc.want {
				t.Fatalf("portable = %v, want %v", got, tc.want)
			}
		})
	}
}

type failedRebalanceWriter struct{ *httptest.ResponseRecorder }

func (w failedRebalanceWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRebalanceFailedNoticeWriteReannounces(t *testing.T) {
	var upstream rebalanceUpstream
	h, cs, db, ts := setupProxy(t, upstream.handler("application/json", rebalanceRespJSON))
	defer ts.Close()
	seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(rebalanceReqBody))
	req.Header.Set("X-Router-Conversation-ID", rebalanceConvID)
	h.ServeHTTP(failedRebalanceWriter{httptest.NewRecorder()}, req)
	got := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
	if got.Header().Get("X-Router-Rebalance") != "pending" {
		t.Fatalf("failed write authorized switch: %v", got.Header())
	}
	auths, _ := upstream.snapshot()
	if len(auths) != 2 || auths[0] != auths[1] {
		t.Fatalf("auths = %v", auths)
	}
}

func TestRebalanceHelperAndScopedResourcesStayPinned(t *testing.T) {
	for _, body := range []string{
		strings.ReplaceAll(rebalanceReqBody, "claude-sonnet-5", "claude-haiku-4-5"),
		`{"model":"claude-sonnet-5","container":"container_test","messages":[]}`,
		`{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"source":{"type":"file","file_id":"file_test"}}]}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			var upstream rebalanceUpstream
			h, cs, db, ts := setupProxy(t, upstream.handler("application/json", rebalanceRespJSON))
			defer ts.Close()
			seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())
			for range 2 {
				rw := postRebalance(t, h, "/v1/messages", body)
				if rw.Code != http.StatusOK || rw.Header().Get("X-Router-Rebalance") != "" {
					t.Fatalf("unexpected rebalance: %d %v", rw.Code, rw.Header())
				}
			}
			auths, bodies := upstream.snapshot()
			if auths[0] != "Bearer "+cs[0].AccessToken || auths[1] != auths[0] || bodies[0] != body || bodies[1] != body {
				t.Fatal("non-portable request changed account or body")
			}
		})
	}
}

const (
	rebalanceConvID = "rebalance-test"

	rebalanceReqBody = `{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"spread the load"}]}`
	// Same turn, streaming; only the stream flag differs.
	rebalanceStreamReq = `{"model":"claude-sonnet-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"spread the load"}]}`
	rebalanceCountBody = `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"spread the load"}]}`

	rebalanceRespJSON   = `{"id":"msg_rebalance","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`
	rebalanceCountResp  = `{"input_tokens":42}`
	rebalanceSSEHead    = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	rebalanceSSETail    = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	rebalanceSpoofValue = "spoofed-from-upstream"
	rebalanceSpoofMsg   = "spoofed upstream notice"
)

// rebalanceUpstream serves a fixed body and records the Authorization header
// and raw body of every request it serves, in arrival order, so a test can
// assert which account served each turn and that the relay was byte-exact.
type rebalanceUpstream struct {
	mu     sync.Mutex
	auths  []string
	bodies []string
}

func (u *rebalanceUpstream) record(auth, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.auths = append(u.auths, auth)
	u.bodies = append(u.bodies, body)
}

func (u *rebalanceUpstream) handler(contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.record(r.Header.Get("Authorization"), string(b))
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}
}

func (u *rebalanceUpstream) snapshot() (auths, bodies []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.auths...), append([]string(nil), u.bodies...)
}

func (u *rebalanceUpstream) authCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.auths)
}

// postRebalance issues one POST against the proxy under the seeded
// conversation key. X-Router-Conversation-ID is the highest-priority
// derivation signal and Anthropic keeps the bare key, so the request lands on
// the seeded 'rebalance-test' row without any metadata in the body.
func postRebalance(t *testing.T, h *Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Router-Conversation-ID", rebalanceConvID)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw
}

// seedRebalancePin builds the qualification on top of setupProxy's two
// same-base-URL Anthropic credentials: a pin bound boundAt unix seconds ago on
// the heavily used source account, with a lightly used sibling to move to.
// boundAt 0 leaves the column unset, so qualification must fall back to
// created_at (seeded 2h old). Each account carries two usage samples 10
// minutes apart with both windows' resets in the future, so neither is
// saturated (the >=100% migration is a different mechanism) but the source is
// clearly the worse home for the pin.
func seedRebalancePin(t *testing.T, db *store.DB, cs []*creds.Credential, boundAt int64) {
	t.Helper()
	now := time.Now()
	// Distinctive labels so a leak of account material into response headers is
	// detectable; setupProxy's "A"/"B" would false-negative against any prose.
	for i, lbl := range []string{"acct-source", "acct-target"} {
		if _, err := db.Exec(`UPDATE credentials SET label=? WHERE id=?`, lbl, cs[i].ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`
		INSERT INTO conversations (id, credential_id, created_at, bound_at, last_seen_at, request_count, status)
		VALUES (?, ?, ?, ?, ?, 0, 'active')`,
		rebalanceConvID, cs[0].ID, now.Add(-2*time.Hour).Unix(), boundAt, now.Unix()); err != nil {
		t.Fatalf("seed conversation (bound_at migration missing?): %v", err)
	}
	seedUsage := func(credID string, fh, sd float64) {
		for _, ago := range []time.Duration{10 * time.Minute, 0} {
			if _, err := db.Exec(`
				INSERT INTO usage_history
				  (credential_id, captured_at, five_hour_pct, five_hour_resets_at,
				   seven_day_pct, seven_day_resets_at, seven_day_sonnet_pct, seven_day_sonnet_resets_at)
				VALUES (?, ?, ?, ?, ?, ?, 0, NULL)`,
				credID, now.Add(-ago).Unix(), fh, now.Add(2*time.Hour).Unix(),
				sd, now.Add(72*time.Hour).Unix()); err != nil {
				t.Fatal(err)
			}
		}
	}
	seedUsage(cs[0].ID, 80, 70) // source: heavily used
	seedUsage(cs[1].ID, 20, 20) // target: plenty of room
}

// assertPinnedTo fails unless the seeded conversation is still pinned to the
// expected credential.
func assertPinnedTo(t *testing.T, db *store.DB, credID string) {
	t.Helper()
	var got string
	if err := db.QueryRow(`SELECT credential_id FROM conversations WHERE id=?`, rebalanceConvID).Scan(&got); err != nil {
		t.Fatalf("conversation %q disappeared: %v", rebalanceConvID, err)
	}
	if got != credID {
		t.Errorf("conversation %q pinned to %s, want %s", rebalanceConvID, got, credID)
	}
}

// assertNoAccountMaterial fails when a notice header discloses account
// material: the message must be human-readable prose, never an internal
// pointer. Labels are the distinctive ones seedRebalancePin installed in the
// database — the in-memory structs still carry setupProxy's "A"/"B", which
// would false-positive against any prose, so they are deliberately not used.
func assertNoAccountMaterial(t *testing.T, cs []*creds.Credential, headerVals ...string) {
	t.Helper()
	secret := []string{"acct-source", "acct-target"}
	for _, c := range cs {
		secret = append(secret, c.ID, c.AccessToken, c.RefreshToken)
	}
	for _, v := range headerVals {
		for _, s := range secret {
			if s != "" && strings.Contains(v, s) {
				t.Errorf("response header leaks account material %q in %q", s, v)
			}
		}
	}
}

// The happy path over JSON. RebalanceSessions is deliberately not set: the
// contract defaults it to true. The notice must come before the switch, the
// switch must land on the other account, the new pin must stick, and both
// directions of the relay must stay byte-identical throughout.
func TestRebalanceNoticeThenSwitchJSON(t *testing.T) {
	up := &rebalanceUpstream{}
	h, cs, db, _ := setupProxy(t, up.handler("application/json", rebalanceRespJSON))
	seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())

	// Turn 1: still served by the pinned account, but the client is warned.
	rw1 := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
	if rw1.Code != http.StatusOK {
		t.Fatalf("turn 1 status = %d body = %s", rw1.Code, rw1.Body.String())
	}
	if got := rw1.Header().Get("X-Router-Rebalance"); got != "pending" {
		t.Errorf("turn 1 X-Router-Rebalance = %q, want %q", got, "pending")
	}
	if got := rw1.Header().Get("X-Router-Message"); got == "" {
		t.Errorf("turn 1 carried no X-Router-Message; the notice must be human-readable")
	}
	assertNoAccountMaterial(t, cs, rw1.Header().Get("X-Router-Rebalance"), rw1.Header().Get("X-Router-Message"))
	if rw1.Body.String() != rebalanceRespJSON {
		t.Errorf("turn 1 response body changed:\n got %s\nwant %s", rw1.Body.String(), rebalanceRespJSON)
	}
	auths, bodies := up.snapshot()
	if len(auths) != 1 || auths[0] != "Bearer "+cs[0].AccessToken {
		t.Errorf("turn 1 upstream auth = %v, want the pinned source account", auths)
	}
	if len(bodies) != 1 || bodies[0] != rebalanceReqBody {
		t.Errorf("turn 1 request body not relayed byte-identical: %q", bodies)
	}
	assertPinnedTo(t, db, cs[0].ID)

	// Turn 2, the first message after turn 1 completed: the switch executes.
	rw2 := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
	if rw2.Code != http.StatusOK {
		t.Fatalf("turn 2 status = %d body = %s", rw2.Code, rw2.Body.String())
	}
	if got := rw2.Header().Get("X-Router-Rebalance"); got != "switched" {
		t.Errorf("turn 2 X-Router-Rebalance = %q, want %q", got, "switched")
	}
	assertNoAccountMaterial(t, cs, rw2.Header().Get("X-Router-Rebalance"), rw2.Header().Get("X-Router-Message"))
	if rw2.Body.String() != rebalanceRespJSON {
		t.Errorf("turn 2 response body changed:\n got %s\nwant %s", rw2.Body.String(), rebalanceRespJSON)
	}
	auths, _ = up.snapshot()
	if len(auths) != 2 || auths[1] != "Bearer "+cs[1].AccessToken {
		t.Errorf("turn 2 upstream auth = %v, want the other account", auths)
	}
	assertPinnedTo(t, db, cs[1].ID)

	// Turn 3: cooldown — the new pin sticks and no further notice is emitted.
	rw3 := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
	if rw3.Code != http.StatusOK {
		t.Fatalf("turn 3 status = %d body = %s", rw3.Code, rw3.Body.String())
	}
	if got := rw3.Header().Get("X-Router-Rebalance"); got != "" {
		t.Errorf("turn 3 X-Router-Rebalance = %q, want none (cooldown)", got)
	}
	if rw3.Body.String() != rebalanceRespJSON {
		t.Errorf("turn 3 response body changed:\n got %s\nwant %s", rw3.Body.String(), rebalanceRespJSON)
	}
	auths, _ = up.snapshot()
	if len(auths) != 3 || auths[2] != "Bearer "+cs[1].AccessToken {
		t.Errorf("turn 3 upstream auth = %v, want the switched-to account", auths)
	}
	assertPinnedTo(t, db, cs[1].ID)
}

// Same handshake over a streaming response, with bound_at left at 0 so
// qualification must fall back to created_at. The SSE bytes must be relayed
// exactly and the notice ordering (pending → switched → cooldown silence) must
// hold for streamed responses too.
func TestRebalanceSSEBodyExactAndNoticeOrder(t *testing.T) {
	sse := rebalanceSSEHead + rebalanceSSETail
	up := &rebalanceUpstream{}
	h, cs, db, _ := setupProxy(t, up.handler("text/event-stream", sse))
	seedRebalancePin(t, db, cs, 0)

	wantNotice := []string{"pending", "switched", ""}
	wantAuth := []string{cs[0].AccessToken, cs[1].AccessToken, cs[1].AccessToken}
	wantRole := []string{"pinned source", "switched-to", "switched-to"}
	for i, notice := range wantNotice {
		rw := postRebalance(t, h, "/v1/messages", rebalanceStreamReq)
		if rw.Code != http.StatusOK {
			t.Fatalf("turn %d status = %d body = %s", i+1, rw.Code, rw.Body.String())
		}
		if got := rw.Header().Get("X-Router-Rebalance"); got != notice {
			t.Errorf("turn %d X-Router-Rebalance = %q, want %q", i+1, got, notice)
		}
		if notice == "pending" && rw.Header().Get("X-Router-Message") == "" {
			t.Errorf("turn %d carried no X-Router-Message; the notice must be human-readable", i+1)
		}
		assertNoAccountMaterial(t, cs, rw.Header().Get("X-Router-Rebalance"), rw.Header().Get("X-Router-Message"))
		if rw.Body.String() != sse {
			t.Errorf("turn %d SSE body changed:\n got %q\nwant %q", i+1, rw.Body.String(), sse)
		}
		auths, _ := up.snapshot()
		if len(auths) != i+1 || auths[i] != "Bearer "+wantAuth[i] {
			t.Errorf("turn %d upstream auth = %v, want the %s account", i+1, auths, wantRole[i])
		}
	}
	assertPinnedTo(t, db, cs[1].ID)
}

// Opting out must be total: no notice headers on any turn, no pin movement.
func TestRebalanceDisabledOptsOut(t *testing.T) {
	up := &rebalanceUpstream{}
	h, cs, db, _ := setupProxy(t, up.handler("application/json", rebalanceRespJSON))
	seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())
	h.RebalanceSessions = false

	for i := range 3 {
		rw := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
		if rw.Code != http.StatusOK {
			t.Fatalf("turn %d status = %d body = %s", i+1, rw.Code, rw.Body.String())
		}
		if got := rw.Header().Get("X-Router-Rebalance"); got != "" {
			t.Errorf("disabled: turn %d emitted X-Router-Rebalance %q", i+1, got)
		}
		if got := rw.Header().Get("X-Router-Message"); got != "" {
			t.Errorf("disabled: turn %d emitted X-Router-Message %q", i+1, got)
		}
		auths, _ := up.snapshot()
		if len(auths) != i+1 || auths[i] != "Bearer "+cs[0].AccessToken {
			t.Errorf("disabled: turn %d was not served by the pinned account: %v", i+1, auths)
		}
	}
	assertPinnedTo(t, db, cs[0].ID)
}

// An error response must never acknowledge the handshake: no pending notice,
// no switched notice — and the client must never see a switched notice that a
// prior successful turn did not warn about.
func TestRebalanceUpstreamErrorNeverAcknowledges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"429", http.StatusTooManyRequests, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`},
		{"500", http.StatusInternalServerError, `{"type":"error","error":{"type":"api_error","message":"boom"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits int
			upstream := func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.Header().Set("Content-Type", "application/json")
				if hits <= 2 {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, rebalanceRespJSON)
			}
			h, cs, db, _ := setupProxy(t, upstream)
			seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())

			for i := range 2 {
				rw := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
				if rw.Code != tc.status {
					t.Fatalf("error turn %d status = %d, want %d (passthrough)", i+1, rw.Code, tc.status)
				}
				if got := rw.Header().Get("X-Router-Rebalance"); got != "" {
					t.Errorf("error turn %d emitted X-Router-Rebalance %q", i+1, got)
				}
				if got := rw.Header().Get("X-Router-Message"); got != "" {
					t.Errorf("error turn %d emitted X-Router-Message %q", i+1, got)
				}
			}

			// First successful turn afterwards: whatever state the failed turns
			// left behind, a switch the client was never warned about must not
			// be announced as done.
			rw := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
			if rw.Code != http.StatusOK {
				t.Fatalf("recovery turn status = %d body = %s", rw.Code, rw.Body.String())
			}
			if got := rw.Header().Get("X-Router-Rebalance"); got == "switched" {
				t.Errorf("recovery turn announced switched without a prior acknowledged pending notice")
			}
		})
	}
}

// count_tokens is not a message generation: it shares the conversation binding
// (and is served by the pinned account) but must neither initiate the
// handshake nor execute a pending switch.
func TestRebalanceCountTokensSharesLeaseWithoutSwitching(t *testing.T) {
	up := &rebalanceUpstream{}
	h, cs, db, _ := setupProxy(t, up.handler("application/json", rebalanceCountResp))
	seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())

	// Before any message turn: no initiation.
	rwCT1 := postRebalance(t, h, "/v1/messages/count_tokens", rebalanceCountBody)
	if rwCT1.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d body = %s", rwCT1.Code, rwCT1.Body.String())
	}
	if got := rwCT1.Header().Get("X-Router-Rebalance"); got != "" {
		t.Errorf("count_tokens initiated the handshake: X-Router-Rebalance %q", got)
	}
	if got := rwCT1.Header().Get("X-Router-Message"); got != "" {
		t.Errorf("count_tokens emitted X-Router-Message %q", got)
	}
	auths, _ := up.snapshot()
	if len(auths) != 1 || auths[0] != "Bearer "+cs[0].AccessToken {
		t.Errorf("count_tokens did not share the pinned binding: %v", auths)
	}

	// The message turn still presents its notice first: count_tokens consumed
	// nothing.
	rw2 := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
	if rw2.Code != http.StatusOK {
		t.Fatalf("message turn status = %d body = %s", rw2.Code, rw2.Body.String())
	}
	if got := rw2.Header().Get("X-Router-Rebalance"); got != "pending" {
		t.Errorf("message turn X-Router-Rebalance = %q, want %q", got, "pending")
	}
	auths, _ = up.snapshot()
	if len(auths) != 2 || auths[1] != "Bearer "+cs[0].AccessToken {
		t.Errorf("message turn upstream auth = %v, want the pinned source account", auths)
	}

	// A count_tokens between notice and switch must not execute the switch.
	rwCT2 := postRebalance(t, h, "/v1/messages/count_tokens", rebalanceCountBody)
	if rwCT2.Code != http.StatusOK {
		t.Fatalf("second count_tokens status = %d body = %s", rwCT2.Code, rwCT2.Body.String())
	}
	if got := rwCT2.Header().Get("X-Router-Rebalance"); got != "" {
		t.Errorf("count_tokens executed the switch: X-Router-Rebalance %q", got)
	}
	if got := rwCT2.Header().Get("X-Router-Message"); got != "" {
		t.Errorf("count_tokens emitted X-Router-Message %q", got)
	}
	auths, _ = up.snapshot()
	if len(auths) != 3 || auths[2] != "Bearer "+cs[0].AccessToken {
		t.Errorf("count_tokens after the notice left the pinned account: %v", auths)
	}
	assertPinnedTo(t, db, cs[0].ID)

	// The next real message executes the switch.
	rw4 := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
	if rw4.Code != http.StatusOK {
		t.Fatalf("final message turn status = %d body = %s", rw4.Code, rw4.Body.String())
	}
	if got := rw4.Header().Get("X-Router-Rebalance"); got != "switched" {
		t.Errorf("final message turn X-Router-Rebalance = %q, want %q", got, "switched")
	}
	auths, _ = up.snapshot()
	if len(auths) != 4 || auths[3] != "Bearer "+cs[1].AccessToken {
		t.Errorf("final message turn upstream auth = %v, want the other account", auths)
	}
	assertPinnedTo(t, db, cs[1].ID)
}

// The router's notice headers are the router's alone: values an upstream sends
// in X-Router-Rebalance / X-Router-Message must be discarded whether or not the
// router emits its own values on that response.
func TestRebalanceDiscardsSpoofedUpstreamNotices(t *testing.T) {
	up := &rebalanceUpstream{}
	base := up.handler("application/json", rebalanceRespJSON)
	h, cs, db, _ := setupProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Router-Rebalance", rebalanceSpoofValue)
		w.Header().Set("X-Router-Message", rebalanceSpoofMsg)
		base(w, r)
	})
	seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())

	assertClean := func(t *testing.T, rw *httptest.ResponseRecorder) {
		t.Helper()
		for _, v := range rw.Header().Values("X-Router-Rebalance") {
			if strings.Contains(v, rebalanceSpoofValue) {
				t.Errorf("spoofed X-Router-Rebalance reached the client: %q", v)
			}
		}
		for _, v := range rw.Header().Values("X-Router-Message") {
			if strings.Contains(v, rebalanceSpoofMsg) {
				t.Errorf("spoofed X-Router-Message reached the client: %q", v)
			}
		}
	}

	// Turn 1: exactly the router's pending value, nothing from the upstream.
	rw1 := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
	if got := rw1.Header().Values("X-Router-Rebalance"); len(got) != 1 || got[0] != "pending" {
		t.Errorf("turn 1 X-Router-Rebalance = %v, want exactly [pending]", got)
	}
	assertClean(t, rw1)

	// Turn 2: exactly the router's switched value.
	rw2 := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
	if got := rw2.Header().Values("X-Router-Rebalance"); len(got) != 1 || got[0] != "switched" {
		t.Errorf("turn 2 X-Router-Rebalance = %v, want exactly [switched]", got)
	}
	assertClean(t, rw2)

	// Turn 3 (cooldown, the router emits nothing): the spoof must still be
	// discarded, not passed through in the router's silence.
	rw3 := postRebalance(t, h, "/v1/messages", rebalanceReqBody)
	if got := rw3.Header().Get("X-Router-Rebalance"); got != "" {
		t.Errorf("turn 3 X-Router-Rebalance = %q, want none (cooldown)", got)
	}
	assertClean(t, rw3)

	auths, _ := up.snapshot()
	if len(auths) != 3 || auths[0] != "Bearer "+cs[0].AccessToken ||
		auths[1] != "Bearer "+cs[1].AccessToken || auths[2] != "Bearer "+cs[1].AccessToken {
		t.Errorf("handshake auth order broken: %v", auths)
	}
}

// A switch may only execute once the previous generation has completed: while
// turn 1 is still streaming from the pinned account, a concurrent turn 2 must
// stay on that account. Coordination is channel-based; the only sleeps are
// bounded timeout guards.
func TestRebalanceInFlightTurnDoesNotSwitch(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release: // already closed
		default:
			close(release)
		}
	})
	firstHit := make(chan struct{}, 1)

	up := &rebalanceUpstream{}
	upstream := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		up.record(r.Header.Get("Authorization"), string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		if up.authCount() == 1 { // the in-flight turn
			_, _ = io.WriteString(w, rebalanceSSEHead) // headers + first event go out
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case firstHit <- struct{}{}:
			default:
			}
			<-release
			_, _ = io.WriteString(w, rebalanceSSETail)
			return
		}
		_, _ = io.WriteString(w, rebalanceSSEHead+rebalanceSSETail)
	}
	h, cs, db, _ := setupProxy(t, upstream)
	seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())

	proxyTS := httptest.NewServer(http.HandlerFunc(h.ServeHTTP))
	t.Cleanup(proxyTS.Close)
	sse := rebalanceSSEHead + rebalanceSSETail

	// newTurn is also used from the turn-1 goroutine, so it never calls
	// t.Fatal; callers report the error through their own path.
	newTurn := func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, proxyTS.URL+"/v1/messages", strings.NewReader(rebalanceStreamReq))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Router-Conversation-ID", rebalanceConvID)
		return req, nil
	}

	// Turn 1: parks inside the upstream after its first event, response in flight.
	type turnResult struct {
		resp *http.Response
		body string
		err  error
	}
	done1 := make(chan turnResult, 1)
	go func() {
		req, err := newTurn()
		if err != nil {
			done1 <- turnResult{err: err}
			return
		}
		resp, err := proxyTS.Client().Do(req)
		if err != nil {
			done1 <- turnResult{err: err}
			return
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		done1 <- turnResult{resp: resp, body: string(b), err: err}
	}()
	select {
	case <-firstHit:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never saw the in-flight turn")
	}

	// Turn 2 arrives while turn 1 is still streaming: no switch, same account.
	req2, err := newTurn()
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := proxyTS.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	body2, err := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("concurrent turn status = %d", resp2.StatusCode)
	}
	if got := resp2.Header.Get("X-Router-Rebalance"); got == "switched" {
		t.Errorf("concurrent turn switched while the previous response was still in flight")
	}
	if string(body2) != sse {
		t.Errorf("concurrent turn SSE body changed:\n got %q\nwant %q", body2, sse)
	}
	auths, _ := up.snapshot()
	if len(auths) != 2 || auths[0] != "Bearer "+cs[0].AccessToken || auths[1] != "Bearer "+cs[0].AccessToken {
		t.Errorf("both concurrent turns must stay on the source account, got %v", auths)
	}
	assertPinnedTo(t, db, cs[0].ID)

	// Let turn 1 finish: it completed on the old account, with the notice.
	close(release)
	var res1 turnResult
	select {
	case res1 = <-done1:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight turn did not complete after release")
	}
	if res1.err != nil {
		t.Fatalf("in-flight turn: %v", res1.err)
	}
	if res1.resp.StatusCode != http.StatusOK {
		t.Fatalf("in-flight turn status = %d", res1.resp.StatusCode)
	}
	if got := res1.resp.Header.Get("X-Router-Rebalance"); got != "pending" {
		t.Errorf("in-flight turn X-Router-Rebalance = %q, want %q", got, "pending")
	}
	if res1.body != sse {
		t.Errorf("in-flight turn SSE body changed:\n got %q\nwant %q", res1.body, sse)
	}

	// Turn 3, the first message after turn 1 completed: the switch executes.
	req3, err := newTurn()
	if err != nil {
		t.Fatal(err)
	}
	resp3, err := proxyTS.Client().Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	body3, err := io.ReadAll(resp3.Body)
	_ = resp3.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("turn 3 status = %d", resp3.StatusCode)
	}
	if got := resp3.Header.Get("X-Router-Rebalance"); got != "switched" {
		t.Errorf("turn 3 X-Router-Rebalance = %q, want %q", got, "switched")
	}
	if string(body3) != sse {
		t.Errorf("turn 3 SSE body changed:\n got %q\nwant %q", body3, sse)
	}
	auths, _ = up.snapshot()
	if len(auths) != 3 || auths[2] != "Bearer "+cs[1].AccessToken {
		t.Errorf("turn 3 upstream auth = %v, want the other account", auths)
	}
	assertPinnedTo(t, db, cs[1].ID)
}
