package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestChatGPTOAuthTransportSanitizesAndRefreshesBearer(t *testing.T) {
	calls := 0
	transport := quotaAwareTransport{
		base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if got := req.Header.Get("Authorization"); got != "Bearer fresh-token" {
				t.Fatalf("Authorization = %q", got)
			}
			raw, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			if body["stream"] != true || body["store"] != false {
				t.Fatalf("required flags missing: %#v", body)
			}
			allowed := map[string]bool{
				"model": true, "input": true, "stream": true, "store": true,
				"tools": true, "tool_choice": true, "parallel_tool_calls": true,
				"reasoning": true, "text": true, "include": true,
			}
			for key := range body {
				if !allowed[key] {
					t.Fatalf("non-allowlisted field %q survived: %#v", key, body)
				}
			}
			input, ok := body["input"].([]any)
			if body["model"] != "gpt-test" || !ok || len(input) != 2 {
				t.Fatalf("supported fields lost: %#v", body)
			}
			developer, ok := input[0].(map[string]any)
			if !ok || developer["role"] != "developer" || developer["content"] != "system rules" {
				t.Fatalf("instructions were not preserved in input: %#v", input)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
		}),
		bearerToken:  func(context.Context) (string, error) { return "fresh-token", nil },
		chatGPTOAuth: true,
	}
	raw := `{"model":"gpt-test","instructions":"system rules","input":[{"role":"user","content":"hi"}],"stream":false,"store":true,"max_output_tokens":10,"temperature":0.2,"metadata":{"x":"y"},"previous_response_id":"r","unknown_preview_field":true}`
	req, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer stale-token")
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}
