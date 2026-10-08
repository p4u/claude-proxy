package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/router"
)

func compactionBody(t *testing.T, texts ...string) string {
	t.Helper()
	messages := make([]map[string]any, 0, len(texts))
	for _, text := range texts {
		messages = append(messages, map[string]any{"role": "user", "content": text})
	}
	b, err := json.Marshal(map[string]any{"model": "claude-sonnet-5", "max_tokens": 1024, "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func compactedText(summary string) string {
	return continuationPrefix + summary + transcriptSuffix + "/tmp/mock/transcript.jsonl" + continuationResume + "\n"
}

func TestClassifyCompaction(t *testing.T) {
	summary := compactionPromptPrefix + compactionPromptLead + "\nDetails follow."
	resumed := compactionBody(t, compactedText("Short summary"))
	for _, tc := range []struct {
		name, class, kind, resumedKind, body string
		source                               router.Source
		phase                                pool.CompactionPhase
		helper, adoption                     bool
	}{
		{"ordinary", "", "", "", rebalanceReqBody, router.SourceHeader, pool.CompactionOrdinary, false, false},
		{"manual summary", "compaction", "manual", "", rebalanceReqBody, router.SourceHeader, pool.CompactionSummary, false, false},
		{"auto summary", "compaction", "auto", "", rebalanceReqBody, router.SourceHeader, pool.CompactionSummary, false, false},
		{"reactive boundary", "main", "", "reactive", resumed, router.SourceMetadataUID, pool.CompactionBoundary, false, true},
		{"manual boundary", "main", "", "manual", rebalanceReqBody, router.SourceHeader, pool.CompactionBoundary, false, false},
		{"auto boundary", "main", "", "auto", rebalanceReqBody, router.SourceHeader, pool.CompactionBoundary, false, false},
		{"headerless summary", "", "", "", compactionBody(t, "old history", summary), router.SourceMetadataUID, pool.CompactionSummary, false, false},
		{"headerless continuation", "", "", "", resumed, router.SourceHeader, pool.CompactionOrdinary, false, true},
		{"summary wins conflict", "compaction", "auto", "manual", resumed, router.SourceHeader, pool.CompactionSummary, false, false},
		{"auxiliary", "auxiliary", "", "manual", resumed, router.SourceHeader, pool.CompactionOrdinary, true, false},
		{"subagent", "subagent", "", "manual", resumed, router.SourceHeader, pool.CompactionOrdinary, true, false},
		{"workflow", "workflow", "", "auto", resumed, router.SourceHeader, pool.CompactionOrdinary, true, false},
		{"unknown class", "future-helper", "", "auto", resumed, router.SourceHeader, pool.CompactionOrdinary, true, false},
		{"unknown kind", "compaction", "future", "", rebalanceReqBody, router.SourceHeader, pool.CompactionOrdinary, true, false},
		{"bare marker", "", "", "manual", rebalanceReqBody, router.SourceHeader, pool.CompactionOrdinary, false, false},
		{"unknown boundary kind", "main", "", "future", rebalanceReqBody, router.SourceHeader, pool.CompactionOrdinary, false, false},
		{"unstable boundary", "main", "", "auto", resumed, router.SourceContentHash, pool.CompactionOrdinary, false, false},
		{"unstable summary freezes", "", "", "", compactionBody(t, summary), router.SourceFallback, pool.CompactionSummary, false, false},
		{"quoted summary prompt", "", "", "", compactionBody(t, "Please explain this:\n"+summary), router.SourceHeader, pool.CompactionOrdinary, false, false},
		{"historical summary prompt", "", "", "", compactionBody(t, summary, "new user turn"), router.SourceHeader, pool.CompactionOrdinary, false, false},
		{"quoted continuation", "", "", "", compactionBody(t, "A quote:\n"+compactedText("Short summary")), router.SourceHeader, pool.CompactionOrdinary, false, false},
		{"late continuation", "", "", "", compactionBody(t, "old user turn", compactedText("Short summary")), router.SourceHeader, pool.CompactionOrdinary, false, false},
		{"malformed body", "main", "", "manual", "{broken", router.SourceHeader, pool.CompactionOrdinary, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			r.Header.Set("X-Claude-Code-Request-Class", tc.class)
			r.Header.Set("X-Claude-Code-Compaction", tc.kind)
			r.Header.Set("X-Claude-Code-Context-Compacted", tc.resumedKind)
			got, helper := classifyCompaction(r, []byte(tc.body), tc.source)
			if got.Phase != tc.phase || helper != tc.helper || (got.Adoption.Bytes > 0) != tc.adoption {
				t.Fatalf("phase=%v helper=%v adoption=%d; want phase=%v helper=%v adoption=%v", got.Phase, helper, got.Adoption.Bytes, tc.phase, tc.helper, tc.adoption)
			}
			if tc.adoption && got.Adoption.Digest != sha256.Sum256([]byte("Short summary")) {
				t.Fatal("adoption did not hash the exact summary")
			}
			if tc.source == router.SourceContentHash || tc.source == router.SourceFallback {
				if got.Request != ([32]byte{}) || got.Prefix != ([32]byte{}) {
					t.Fatal("unstable identities must not create correlation state")
				}
			}
		})
	}
}

func TestCompactionContentBlocksAndRetryFingerprint(t *testing.T) {
	// CLI puts reminder blocks before the continuation and local command output
	// after it. Cache markers move between requests, but are not history identity.
	body := func(cache, stream bool) []byte {
		blocks := []map[string]any{
			{"type": "text", "text": "<system-reminder>environment</system-reminder>\n"},
			{"type": "text", "text": compactedText("Short summary")},
			{"type": "text", "text": "<local-command-caveat>local command</local-command-caveat>"},
		}
		if cache {
			blocks[1]["cache_control"] = map[string]string{"type": "ephemeral"}
		}
		b, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5", "stream": stream, "messages": []any{map[string]any{"role": "user", "content": blocks}}})
		return b
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	a, _ := classifyCompaction(r, body(true, true), router.SourceHeader)
	b, _ := classifyCompaction(r, body(false, false), router.SourceHeader)
	if a.Adoption.Bytes == 0 || a.Adoption != b.Adoption || a.Prefix != b.Prefix {
		t.Fatal("reminders/cache markers hid or changed the compaction prefix")
	}
	r.Header.Set("X-Claude-Code-Request-Class", "main")
	r.Header.Set("X-Claude-Code-Context-Compacted", "manual")
	plain := []byte(rebalanceReqBody)
	x, _ := classifyCompaction(r, plain, router.SourceHeader)
	y, _ := classifyCompaction(r, []byte(strings.Replace(rebalanceReqBody, `"max_tokens":16`, `"stream":true,"max_tokens":32`, 1)), router.SourceHeader)
	if x.Request == ([32]byte{}) || x.Request != y.Request {
		t.Fatal("streaming fallback changed logical request identity")
	}
	for _, path := range []string{"/v1/messages/count_tokens", "/v1/models"} {
		r.URL.Path = path
		got, helper := classifyCompaction(r, body(false, false), router.SourceHeader)
		if !helper || got.Adoption.Bytes != 0 {
			t.Fatal("helper consumed compaction evidence")
		}
	}
}

// summaryWire builds a real Messages-shaped response, not the minimal fixtures
// that intentionally suffice for the ordinary notice handshake.
func summaryWire(t *testing.T, text string, stream bool) string {
	t.Helper()
	message := map[string]any{"id": "msg_summary", "type": "message", "role": "assistant", "model": "claude-sonnet-5", "content": []any{}, "stop_reason": nil}
	if !stream {
		message["content"] = []any{map[string]string{"type": "text", "text": text}}
		message["stop_reason"] = "end_turn"
		b, _ := json.Marshal(message)
		return string(b)
	}
	events := []map[string]any{
		{"type": "message_start", "message": message},
		{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}},
		{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": text}},
		{"type": "content_block_stop", "index": 0},
		{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}},
		{"type": "message_stop"},
	}
	var out strings.Builder
	for _, event := range events {
		b, _ := json.Marshal(event)
		fmt.Fprintf(&out, "event: %s\ndata: %s\n\n", event["type"], b)
	}
	return out.String()
}

func postCompaction(t *testing.T, h *Handler, body, class, kind, resumed string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("X-Router-Conversation-ID", rebalanceConvID)
	r.Header.Set("X-Claude-Code-Request-Class", class)
	r.Header.Set("X-Claude-Code-Compaction", kind)
	r.Header.Set("X-Claude-Code-Context-Compacted", resumed)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCompactionHandoffEndToEnd(t *testing.T) {
	for _, headers := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, priorNotice := range []bool{false, true} {
				t.Run(fmt.Sprintf("headers=%v/stream=%v/priorNotice=%v", headers, stream, priorNotice), func(t *testing.T) {
					up := &rebalanceUpstream{}
					summary := "<analysis>private reasoning</analysis>\n\n<summary>\nKeep all user constraints.\n</summary>"
					wire := summaryWire(t, summary, stream)
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
					}
					h, cs, db, _ := setupProxy(t, up.handler(contentType, wire))
					seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())
					h.PromptRetentionDays = 0 // inference must not need opt-in capture
					if priorNotice {
						postRebalance(t, h, "/v1/messages", rebalanceReqBody)
					}
					summaryBody := compactionBody(t, "old history", compactionPromptPrefix+compactionPromptLead)
					resumedBody := compactionBody(t, compactedText("Summary:\nKeep all user constraints."))
					class, kind := "", ""
					if headers {
						class, kind = "compaction", "auto"
						// Prove hints work independently of the body templates.
						summaryBody = compactionBody(t, "old history", "a new summarization template")
						resumedBody = compactionBody(t, "a different compacted input format")
					}
					w := postCompaction(t, h, summaryBody, class, kind, "")
					if w.Code != 200 || w.Header().Get("X-Router-Rebalance") == "switched" || w.Body.String() != wire {
						t.Fatalf("summary moved or was modified: status=%d headers=%v", w.Code, w.Header())
					}
					assertPinnedTo(t, db, cs[0].ID)
					// Helpers and a concurrent old-history request must not consume
					// the boundary or rush a move between summary and adoption.
					postCompaction(t, h, resumedBody, "auxiliary", "", "auto")
					postRebalance(t, h, "/v1/messages/count_tokens", resumedBody)
					postRebalance(t, h, "/v1/messages", rebalanceReqBody)
					assertPinnedTo(t, db, cs[0].ID)
					class, kind = "", ""
					if headers {
						class, kind = "main", "auto"
					}
					w = postCompaction(t, h, resumedBody, class, "", kind)
					if w.Code != 200 || w.Header().Get("X-Router-Rebalance") != "switched" || w.Body.String() != wire {
						t.Fatalf("first compacted turn did not switch byte-exactly: status=%d headers=%v", w.Code, w.Header())
					}
					assertPinnedTo(t, db, cs[1].ID)
					w = postCompaction(t, h, resumedBody, class, "", kind)
					if w.Header().Get("X-Router-Rebalance") != "" {
						t.Fatal("retry rearmed the boundary")
					}
					auths, bodies := up.snapshot()
					for _, auth := range auths[:len(auths)-2] {
						if auth != "Bearer "+cs[0].AccessToken {
							t.Fatal("pre-compaction work left its source")
						}
					}
					if bodies[len(bodies)-1] != resumedBody || auths[len(auths)-1] != "Bearer "+cs[1].AccessToken {
						t.Fatal("resumed request changed or did not stay on destination")
					}
					var count int
					var reason string
					if err := db.QueryRow(`SELECT COUNT(*), reason FROM routing_event WHERE kind='switched'`).Scan(&count, &reason); err != nil {
						t.Fatal(err)
					}
					wantReason := "compaction-inferred"
					if headers {
						wantReason = "compaction-header"
					}
					if count != 1 || reason != wantReason {
						t.Fatalf("switch audit count=%d reason=%q", count, reason)
					}
					if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_message`).Scan(&count); err != nil || count != 0 {
						t.Fatalf("summary detection persisted text: count=%d err=%v", count, err)
					}
				})
			}
		}
	}
}

func TestCompactionFailedSummaryCannotInferBoundary(t *testing.T) {
	valid := summaryWire(t, "short summary", false)
	for _, tc := range []struct{ name, contentType, wire string }{
		{"token cutoff", "application/json", strings.Replace(valid, "end_turn", "max_tokens", 1)},
		{"refusal", "application/json", strings.Replace(valid, "end_turn", "refusal", 1)},
		{"truncated JSON", "application/json", valid[:len(valid)-2]},
		{"truncated SSE", "text/event-stream", rebalanceSSEHead + "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"short summary\"}}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, cs, db, _ := setupProxy(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.wire)
			})
			seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())
			postRebalance(t, h, "/v1/messages", compactionBody(t, "old history", compactionPromptPrefix+compactionPromptLead))
			w := postRebalance(t, h, "/v1/messages", compactionBody(t, compactedText("short summary")))
			if w.Header().Get("X-Router-Rebalance") == "switched" {
				t.Fatal("failed summary created a handoff boundary")
			}
			assertPinnedTo(t, db, cs[0].ID)
		})
	}
}

func TestCompactionAffinitySurvivesRemovedResources(t *testing.T) {
	for _, hints := range []bool{false, true} {
		t.Run(fmt.Sprintf("headers=%v", hints), func(t *testing.T) {
			up := &rebalanceUpstream{}
			h, cs, db, _ := setupProxy(t, up.handler("application/json", summaryWire(t, "Keep constraints.", false)))
			seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())
			postRebalance(t, h, "/v1/messages", `{"model":"claude-sonnet-5","container":"container_mock","messages":[{"role":"user","content":"old history"}]}`)
			h.pool = pool.New(db) // the durable latch, not a request marker, protects resources
			class, kind := "", ""
			if hints {
				class, kind = "compaction", "manual"
			}
			postCompaction(t, h, compactionBody(t, "old history", compactionPromptPrefix+compactionPromptLead), class, kind, "")
			if hints {
				class, kind = "main", "manual"
			}
			for range 2 {
				w := postCompaction(t, h, compactionBody(t, compactedText("Keep constraints.")), class, "", kind)
				if w.Code != http.StatusOK || w.Header().Get("X-Router-Rebalance") != "" {
					t.Fatal("compaction cleared account-bound history")
				}
			}
			assertPinnedTo(t, db, cs[0].ID)
		})
	}
}

func TestCompactionIncompleteDeliveryCannotProveSummary(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(*httptest.ResponseRecorder, context.CancelFunc) http.ResponseWriter
	}{
		{"short write", func(w *httptest.ResponseRecorder, _ context.CancelFunc) http.ResponseWriter {
			return shortRebalanceWriter{w}
		}},
		{"flush failure", func(w *httptest.ResponseRecorder, _ context.CancelFunc) http.ResponseWriter {
			return failedFlushRebalanceWriter{w}
		}},
		{"canceled client", func(w *httptest.ResponseRecorder, cancel context.CancelFunc) http.ResponseWriter {
			return cancelledRebalanceWriter{w, cancel}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &rebalanceUpstream{}
			h, cs, db, _ := setupProxy(t, up.handler("application/json", summaryWire(t, "Keep constraints.", false)))
			seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())
			postRebalance(t, h, "/v1/messages", rebalanceReqBody) // an already acknowledged plan
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(compactionBody(t, "old history", compactionPromptPrefix+compactionPromptLead))).WithContext(ctx)
			r.Header.Set("X-Router-Conversation-ID", rebalanceConvID)
			h.ServeHTTP(tc.wrap(httptest.NewRecorder(), cancel), r)
			w := postRebalance(t, h, "/v1/messages", compactionBody(t, compactedText("Keep constraints.")))
			if w.Header().Get("X-Router-Rebalance") == "switched" {
				t.Fatal("failed client delivery established summary proof")
			}
			assertPinnedTo(t, db, cs[0].ID)
		})
	}
}

func TestCompactionNoNoticeAndInferenceRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			up := &rebalanceUpstream{}
			h, cs, db, _ := setupProxy(t, up.handler("application/json", summaryWire(t, "Keep constraints.", false)))
			seedRebalancePin(t, db, cs, time.Now().Add(-2*time.Hour).Unix())
			class, kind := "main", "auto"
			if restart {
				postRebalance(t, h, "/v1/messages", compactionBody(t, "old history", compactionPromptPrefix+compactionPromptLead))
				h.pool = pool.New(db)
				class, kind = "", ""
			}
			// Without an observed, acknowledged plan there is no special move.
			// Headerless restart may announce an ordinary plan, but not infer one.
			w := postCompaction(t, h, compactionBody(t, compactedText("Keep constraints.")), class, "", kind)
			if w.Header().Get("X-Router-Rebalance") == "switched" {
				t.Fatal("missing in-memory correlation manufactured a handoff")
			}
			assertPinnedTo(t, db, cs[0].ID)
		})
	}
}

func TestContinuationSuffix(t *testing.T) {
	// NEL is not stripped by the client's ECMAScript trim; BOM is.
	for _, text := range []string{"\u0085summary\u0085", "Summary:\nKeep constraints."} {
		if got := continuationProof(compactedText(text)); got.Digest != sha256.Sum256([]byte(text)) {
			t.Fatal("continuation changed normalized summary whitespace")
		}
	}
	for _, suffix := range []string{
		"", transcriptSuffix + "/tmp/log.jsonl", continuationResume,
		transcriptSuffix + "/tmp/log.jsonl\n\nRecent messages are preserved verbatim." + continuationResume,
	} {
		got := continuationProof(continuationPrefix + "Summary:\nKeep constraints." + suffix)
		if got.Bytes != len("Summary:\nKeep constraints.") || got.Digest != sha256.Sum256([]byte("Summary:\nKeep constraints.")) {
			t.Errorf("suffix %q: wrong proof %+v", suffix, got)
		}
	}
	for _, text := range []string{
		continuationPrefix,
		continuationPrefix + "summary" + transcriptSuffix,
		continuationPrefix + "summary" + transcriptSuffix + "/tmp/log\narbitrary user instruction",
		"quoted " + compactedText("summary"),
	} {
		if got := continuationProof(text); got.Bytes != 0 {
			t.Errorf("accepted malformed continuation %q", text)
		}
	}
}
