package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/p4u/claude-proxy/internal/pool"
	"github.com/p4u/claude-proxy/internal/router"
)

// These are wire templates, not a natural-language classifier. Unknown client
// versions lose the optimization rather than guessing from a smaller body or
// a quoted marker. The public hint headers do not depend on these templates.
const compactionPromptPrefix = `CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.

- Do NOT use Read, Bash, Grep, Glob, Edit, Write, or ANY other tool.
- You already have all the context you need in the conversation above.
- Tool calls will be REJECTED and will waste your only turn — you will fail the task.
- Your entire response must be plain text: an <analysis> block followed by a <summary> block.

`

const compactionPromptLead = "Your task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests and your previous actions."
const continuationPrefix = "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\n"
const transcriptSuffix = "\n\nIf you need specific details from before compaction (like exact code snippets, error messages, or content you generated), read the full transcript at: "
const continuationResume = "\nContinue the conversation from where it left off without asking the user any further questions. Resume directly — do not acknowledge the summary, do not recap what was happening, do not preface with \"I'll continue\" or similar. Pick up the last task as if the break never happened."
const recentMessagesSuffix = "\n\nRecent messages are preserved verbatim."

type compactionMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// classifyCompaction returns hints for the pool's synchronized, one-use state
// machine. A body continuation alone is only a candidate: the pool must match
// it to a completed summary from the same stable binding before it can switch.
// The boolean marks helpers which must not prepare, execute, or consume evidence.
func classifyCompaction(r *http.Request, body []byte, source router.Source) (pool.CompactionRequest, bool) {
	var hint pool.CompactionRequest
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || strings.Contains(requestModel(body), "haiku") {
		return hint, true
	}
	class := r.Header.Get("X-Claude-Code-Request-Class")
	summaryHeader := class == "compaction" && compactionKind(r.Header.Get("X-Claude-Code-Compaction"))
	if class != "" && class != "main" && !summaryHeader {
		return hint, true
	}
	var input struct {
		Model    string              `json:"model"`
		System   json.RawMessage     `json:"system"`
		Messages []compactionMessage `json:"messages"`
	}
	if json.Unmarshal(body, &input) != nil || len(input.Messages) == 0 {
		return hint, true
	}
	last := input.Messages[len(input.Messages)-1]
	if summaryHeader || last.Role == "user" && strings.HasPrefix(compactionText(last.Content), compactionPromptPrefix+compactionPromptLead) {
		hint.Phase = pool.CompactionSummary
	}
	// Content-derived identities can change precisely when history is compacted.
	// Never join those conversations heuristically or treat them as the old pin.
	if source != router.SourceHeader && source != router.SourceMetadataUID {
		return hint, false
	}
	first := input.Messages[0]
	if first.Role != "user" {
		return hint, false
	}
	prefix := compactionText(first.Content)
	if hint.Phase != pool.CompactionSummary {
		if class == "main" && compactionKind(r.Header.Get("X-Claude-Code-Context-Compacted")) && r.Header.Get("X-Claude-Code-Compaction") == "" {
			hint.Phase = pool.CompactionBoundary
		}
		hint.Adoption = continuationProof(prefix)
	}
	if hint.Phase != pool.CompactionOrdinary || hint.Adoption.Bytes > 0 {
		// Exclude transport/retry knobs (stream, max_tokens) and user metadata.
		// RawMessage marshaling removes insignificant whitespace; prefix identity
		// additionally survives moving cache_control markers and later turns.
		logical, err := json.Marshal(input)
		if err == nil && prefix != "" {
			hint.Request = sha256.Sum256(logical)
			hint.Prefix = sha256.Sum256([]byte(prefix))
		}
	}
	return hint, false
}

func compactionKind(kind string) bool {
	return kind == "manual" || kind == "auto" || kind == "reactive"
}

// compactionText selects the first actual text block, after the CLI's leading
// reminder blocks. It never searches tool results or arbitrary later blocks.
// This also excludes cache_control from prefix identity.
func compactionText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	for _, block := range blocks {
		if block.Type != "text" {
			return ""
		}
		trimmed := strings.TrimSpace(block.Text)
		if strings.HasPrefix(trimmed, "<system-reminder>") && strings.HasSuffix(trimmed, "</system-reminder>") {
			continue
		}
		return block.Text
	}
	return ""
}

// continuationProof hashes only the normalized summary embedded in the CLI's
// leading continuation block. The optional known suffix is checked, never
// logged or persisted (in particular, the client's transcript path is private).
func continuationProof(text string) pool.SummaryProof {
	var empty pool.SummaryProof
	if !strings.HasPrefix(text, continuationPrefix) {
		return empty
	}
	summary := strings.TrimSuffix(strings.TrimPrefix(text, continuationPrefix), "\n")
	// Search from the end because a summary can itself quote an older wrapper.
	cut := len(summary)
	for _, suffix := range []string{transcriptSuffix, recentMessagesSuffix, continuationResume} {
		if pos := strings.LastIndex(summary, suffix); pos >= 0 && pos < cut {
			cut = pos
		}
	}
	if !validContinuationSuffix(summary[cut:]) {
		return empty
	}
	summary = summary[:cut]
	if summary == "" || len(summary) > summaryTextCap || strings.TrimFunc(summary, summaryTrimSpace) != summary {
		return empty
	}
	return pool.SummaryProof{Digest: sha256.Sum256([]byte(summary)), Bytes: len(summary)}
}

func validContinuationSuffix(suffix string) bool {
	if strings.HasPrefix(suffix, transcriptSuffix) {
		suffix = strings.TrimPrefix(suffix, transcriptSuffix)
		path, rest, _ := strings.Cut(suffix, "\n")
		if path == "" || len(path) > 4096 {
			return false
		}
		suffix = ""
		if rest != "" {
			suffix = "\n" + rest
		}
	}
	suffix = strings.TrimPrefix(suffix, recentMessagesSuffix)
	suffix = strings.TrimPrefix(suffix, continuationResume)
	return suffix == ""
}
