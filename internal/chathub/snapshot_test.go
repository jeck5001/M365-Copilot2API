package chathub

import (
	"strings"
	"testing"
)

// collect drives the real snapshotEmitter and reports what a client would see.
func collect() (*snapshotEmitter, *strings.Builder) {
	var got strings.Builder
	e := newSnapshotEmitter(got.String, func(d string) error {
		got.WriteString(d)
		return nil
	})
	return e, &got
}

func TestSnapshotEmitterGrowingSnapshots(t *testing.T) {
	e, got := collect()
	for _, s := range []string{"He", "Hello", "Hello wor", "Hello world"} {
		if err := e.Add("msg#0", s); err != nil {
			t.Fatal(err)
		}
	}
	if got.String() != "Hello world" {
		t.Fatalf("got %q", got.String())
	}
}

// Two bot messages in the same resent messages array. A single shared snapshot
// variable re-emitted both in full on every frame; per-source state must not.
//
// A linear delta stream cannot group two messages that grow at the same time,
// so the fragments arrive interleaved. That is a protocol limit, not a defect
// here; what matters is that each fragment is emitted exactly once. In practice
// only one bot message grows at a time, covered by the amplification test.
func TestSnapshotEmitterInterleavedMessagesDoNotDuplicate(t *testing.T) {
	e, got := collect()
	plan := []string{"Plan:", "Plan: read", "Plan: read files"}
	answer := []string{"Ans:", "Ans: go test", "Ans: go test failed"}
	for i := range plan {
		if err := e.Add("msg#0", plan[i]); err != nil {
			t.Fatal(err)
		}
		if err := e.Add("msg#1", answer[i]); err != nil {
			t.Fatal(err)
		}
	}
	// Every increment appears once, so the stream is exactly the two final
	// texts' length with no repetition.
	final := plan[len(plan)-1] + answer[len(answer)-1]
	if got.Len() != len(final) {
		t.Fatalf("got %d bytes (%q), want %d", got.Len(), got.String(), len(final))
	}
	for _, frag := range []string{"Plan:", " read", " files", "Ans:", " go test", " failed"} {
		if n := strings.Count(got.String(), frag); n != 1 {
			t.Fatalf("fragment %q emitted %d times in %q", frag, n, got.String())
		}
	}
}

// The 857KB regression: each frame resends both messages in full, and the
// answer grows by one sentence. Output must stay proportional to the content.
func TestSnapshotEmitterNoAmplificationAcrossFrames(t *testing.T) {
	e, got := collect()
	preamble := "I will inspect the repository in read-only mode."
	answer := ""
	for i := 0; i < 40; i++ {
		answer += "Sentence number " + string(rune('a'+i%26)) + ". "
		if err := e.Add("msg:plan", preamble); err != nil {
			t.Fatal(err)
		}
		if err := e.Add("msg:ans", answer); err != nil {
			t.Fatal(err)
		}
	}
	want := preamble + answer
	if got.String() != want {
		t.Fatalf("amplified to %d bytes, want %d\ngot: %q", got.Len(), len(want), got.String())
	}
}

// The cursor channel is a fallback: once a bot message arrives it carries the
// whole answer, so continuing to emit cursor deltas doubles the reply.
func TestSnapshotEmitterCursorStopsOnceSnapshotsStart(t *testing.T) {
	e, got := collect()
	if err := e.AddCursor("##"); err != nil {
		t.Fatal(err)
	}
	if err := e.Add("msg#0", "## 总体评价"); err != nil {
		t.Fatal(err)
	}
	// Everything after the first snapshot must come from the snapshot channel.
	for _, d := range []string{"\n\n", "当前", "项目"} {
		if err := e.AddCursor(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Add("msg#0", "## 总体评价\n\n当前项目"); err != nil {
		t.Fatal(err)
	}
	if got.String() != "## 总体评价\n\n当前项目" {
		t.Fatalf("got %q", got.String())
	}
}

// A turn that only ever sends writeAtCursor still streams: the channel is
// already incremental, so the deltas are emitted verbatim.
func TestSnapshotEmitterCursorOnlyTurnStreamsVerbatim(t *testing.T) {
	e, got := collect()
	for _, d := range []string{"Hello", " ", "world"} {
		if err := e.AddCursor(d); err != nil {
			t.Fatal(err)
		}
	}
	if got.String() != "Hello world" {
		t.Fatalf("got %q", got.String())
	}
}

// The captured contract: a bot message's snapshots are strictly prefix
// cumulative, so each delta is an exact slice. 19 turns, no exception.
func TestSnapshotEmitterPrefixCumulativeSnapshotsAreExact(t *testing.T) {
	e, got := collect()
	full := "这是一个 Go 网关项目。它把私有协议转换为兼容接口。承担会话复用与流式转换。"
	runes := []rune(full)
	for i := 3; i <= len(runes); i += 3 {
		if err := e.Add("msg:m1", string(runes[:i])); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Add("msg:m1", full); err != nil {
		t.Fatal(err)
	}
	if got.String() != full {
		t.Fatalf("got  %q\nwant %q", got.String(), full)
	}
}

// A snapshot that restates instead of extending is outside the captured
// protocol. If it ever happens the reply must survive intact, even at the cost
// of repeating text: a duplicate is cosmetic, a deletion corrupts the answer.
func TestSnapshotEmitterRestatementNeverDeletesText(t *testing.T) {
	e, got := collect()
	first := "项目是 Go 1.23 实现的有状态协议网关。"
	// Rewinds and rewrites: shares only a trailing run with what was streamed.
	second := "。它覆盖 OpenAI 与 Anthropic 两套接口。"
	if err := e.Add("msg:m1", first); err != nil {
		t.Fatal(err)
	}
	if err := e.Add("msg:m1", second); err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{first, second} {
		if !strings.Contains(got.String(), frag) {
			t.Fatalf("dropped %q from %q", frag, got.String())
		}
	}
}

// Replays the interleaving that the frame capture actually shows, which is what
// every earlier round got wrong: the cursor channel and the snapshot channel
// carry the same answer without agreeing on it. In the captured turn the cursor
// stream omitted 1506 of 4932 characters, so no overlap arithmetic can reconcile
// the two. The stream must equal the snapshot channel exactly.
func TestSnapshotEmitterCursorAndSnapshotsDoNotBothStream(t *testing.T) {
	e, got := collect()
	full := "## 总体评价\n\n当前项目是一个功能较完整的 Go 协议网关，覆盖三套兼容接口。"
	runes := []rune(full)
	// Cursor deltas skip characters, exactly as observed; snapshots do not.
	for i := 1; i <= len(runes); i++ {
		if i%4 != 0 {
			if err := e.AddCursor(string(runes[i-1])); err != nil {
				t.Fatal(err)
			}
		}
		if i%3 == 0 {
			if err := e.Add("msg:m1", string(runes[:i])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.Add("msg:m1", full); err != nil {
		t.Fatal(err)
	}
	if got.String() != full {
		t.Fatalf("got %d bytes, want %d\ngot  %q\nwant %q", got.Len(), len(full), got.String(), full)
	}
}

// Scale check against the shape of the 2180-frame turn: a long answer whose
// snapshot is resent in full on every frame while the cursor channel chatters
// alongside it. The log for that turn read streamed_text=11051 against an 8630
// byte answer, so output length is the assertion that matters.
func TestSnapshotEmitterNoAmplificationAtTurnScale(t *testing.T) {
	e, got := collect()
	var text strings.Builder
	for i := 0; i < 300; i++ {
		clause := "句子 " + string(rune('a'+i%26)) + "。"
		text.WriteString(clause)
		// The cursor channel repeats the same content on its own schedule.
		if i%3 != 0 {
			if err := e.AddCursor(clause); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.Add("msg:m1", text.String()); err != nil {
			t.Fatal(err)
		}
	}
	if got.String() != text.String() {
		t.Fatalf("got %d bytes, want %d\ngot  %q", got.Len(), text.Len(), got.String())
	}
}

// Trimming on a coincidental overlap deletes real text. Chinese prose repeats
// short runs constantly — a trailing "，" or a shared character — and treating
// those as proof the delta was already streamed dropped characters from the
// middle of answers ("项目是" rendered as "项 "). A duplicate is cosmetic; a
// deletion corrupts the reply, so short matches must not trigger a trim.
func TestSnapshotEmitterDoesNotTrimOnCoincidentalOverlap(t *testing.T) {
	e, got := collect()
	// Both clauses end and begin with common characters, but neither restates
	// the other.
	first := "项目是 Go 1.23 实现的有状态协议网关，"
	second := "，覆盖 OpenAI Chat Completions 和 Anthropic Messages。"
	if err := e.Add("cursor", first); err != nil {
		t.Fatal(err)
	}
	if err := e.Add("msg#0", second); err != nil {
		t.Fatal(err)
	}
	// Exact equality, not fragment presence: the observed damage was single
	// characters vanishing, which every fragment check still passes.
	if got.String() != first+second {
		t.Fatalf("got  %q\nwant %q", got.String(), first+second)
	}
}

// A frame boundary can split a multi-byte character, so the two strings being
// compared are not guaranteed to be valid UTF-8. A byte-wise overlap could then
// land mid-character and emit a fragment of one, corrupting the text from that
// point on. The cut must always fall on a character boundary.
func TestOverlapLenNeverCutsInsideACharacter(t *testing.T) {
	// cur ends with the first two bytes of a 3-byte character; snapshot begins
	// with the whole character. A byte-wise match would accept n=2.
	cur := "已完成\xe3\x80"
	snapshot := "\xe3\x80\x82覆盖范围"
	if n := overlapLen(cur, snapshot); n != 0 {
		t.Fatalf("overlapLen = %d, want 0; cutting there emits %q", n, snapshot[n:])
	}
}

// The completion frame restates the finished answer, so it is preferred over the
// reassembled delta stream.
func TestCompletionTextPrefersFinalBotMessage(t *testing.T) {
	item := map[string]any{
		"result": map[string]any{"value": "Success", "message": ""},
		"messages": []any{
			map[string]any{"author": "bot", "messageType": "Progress", "text": "正在分析"},
			map[string]any{"author": "user", "text": "分析下当前项目"},
			map[string]any{"author": "bot", "text": "项目是 Go 1.23 实现的有状态协议网关。"},
		},
	}
	if got := completionText(item); got != "项目是 Go 1.23 实现的有状态协议网关。" {
		t.Fatalf("got %q", got)
	}
}

// A turn can carry a preamble message and then the answer, both of which were
// streamed to the client. Returning only the last one made the non-streaming
// reply shorter than the streamed one and dropped the preamble.
func TestCompletionTextJoinsEveryProseMessage(t *testing.T) {
	item := map[string]any{
		"messages": []any{
			map[string]any{"author": "bot", "messageType": "Progress", "text": "正在准备"},
			map[string]any{"author": "bot", "text": "我先读取附件中的实际请求。"},
			map[string]any{"author": "bot", "messageType": "Progress", "text": "**Investigating**"},
			map[string]any{"author": "bot", "text": "我目前无法读取该附件。"},
		},
	}
	want := "我先读取附件中的实际请求。我目前无法读取该附件。"
	if got := completionText(item); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestCompletionTextEmptyWhenNoBotProse(t *testing.T) {
	cases := []map[string]any{
		{},
		{"messages": []any{}},
		{"messages": []any{map[string]any{"author": "bot", "messageType": "Progress", "text": "正在分析"}}},
		{"messages": []any{map[string]any{"author": "bot", "text": "   "}}},
	}
	for i, item := range cases {
		if got := completionText(item); got != "" {
			t.Errorf("case %d: got %q, want empty", i, got)
		}
	}
}

func TestOverlapLen(t *testing.T) {
	cases := []struct {
		cur, snap string
		want      int
	}{
		{"abcdef", "defgh", 3},
		{"abc", "abcdef", 3},
		{"abc", "xyz", 0},
		{"", "abc", 0},
		{"abc", "", 0},
		{"aaa", "aaaa", 3},
	}
	for _, c := range cases {
		if n := overlapLen(c.cur, c.snap); n != c.want {
			t.Errorf("overlapLen(%q, %q) = %d, want %d", c.cur, c.snap, n, c.want)
		}
	}
}

func TestSnapshotEmitterIgnoresRepeatAndEmpty(t *testing.T) {
	e, got := collect()
	for _, s := range []string{"same", "same", "", "same"} {
		if err := e.Add("msg#0", s); err != nil {
			t.Fatal(err)
		}
	}
	if got.String() != "same" {
		t.Fatalf("got %q", got.String())
	}
}

func TestMessageSourceKeys(t *testing.T) {
	if k := messageSource(map[string]any{"messageId": "m1"}, 3); k != "msg:m1" {
		t.Fatalf("got %q", k)
	}
	if k := messageSource(map[string]any{"id": "m2"}, 3); k != "msg:m2" {
		t.Fatalf("got %q", k)
	}
	if k := messageSource(map[string]any{"messageId": ""}, 2); k != "msg#2" {
		t.Fatalf("got %q", k)
	}
	a, b := messageSource(map[string]any{}, 0), messageSource(map[string]any{}, 1)
	if a == b {
		t.Fatalf("distinct positions collided: %q", a)
	}
}
