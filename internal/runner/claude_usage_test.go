package runner

import "testing"

func TestClaudeInterruptedMessageUsage(t *testing.T) {
	p := &claudeParser{}
	// Claude can repeat a message as its content/usage becomes complete.
	for _, line := range []string{
		`{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":100,"cache_read_input_tokens":500,"output_tokens":2},"content":[]}}`,
		`{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":100,"cache_read_input_tokens":500,"output_tokens":8},"content":[]}}`,
		`{"type":"assistant","message":{"id":"m1","usage":{"output_tokens":8},"content":[]}}`,
		`{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":50,"output_tokens":5},"content":[]}}`,
	} {
		p.Line([]byte(line))
	}
	var r Result
	p.Finish(&r)
	if r.Tokens.Total() != 163 || r.Tokens.Cached != 500 || !r.Tokens.Incomplete {
		t.Fatalf("lost/doubled interrupted usage: %+v", r.Tokens)
	}
	p.Line([]byte(`{"type":"result","subtype":"success","usage":{"input_tokens":200,"output_tokens":20},"total_cost_usd":0.5,"result":"done"}`))
	p.Finish(&r)
	if r.Tokens.Total() != 220 || r.Tokens.CostUSD != 0.5 || r.Tokens.Incomplete {
		t.Fatalf("final aggregate not authoritative: %+v", r.Tokens)
	}
}
