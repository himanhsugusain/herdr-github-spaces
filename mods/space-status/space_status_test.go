package main

import (
	"reflect"
	"testing"
)

func TestTallyChecks(t *testing.T) {
	checks := []any{
		map[string]any{"conclusion": "FAILURE", "name": "lint"},
		map[string]any{"conclusion": "CANCELLED", "name": "cancelled"},
		map[string]any{"conclusion": "SKIPPED", "name": "optional"},
		map[string]any{"conclusion": "SUCCESS", "name": "unit"},
		map[string]any{"status": "IN_PROGRESS", "name": "build"},
	}
	got := tallyChecks(checks)
	want := checkTally{Failed: 2, Pending: 1, Passed: 1, Skipped: 1, Total: 5}
	if got != want {
		t.Fatalf("tallyChecks() = %+v, want %+v", got, want)
	}
}

func TestTallyPRChecksDeduplicatesFailures(t *testing.T) {
	got := tallyPRChecks([]any{
		map[string]any{"conclusion": "FAILURE", "name": "lint"},
		map[string]any{"conclusion": "FAILURE", "name": "lint"},
		map[string]any{"conclusion": "SUCCESS", "name": "unit"},
		map[string]any{"status": "IN_PROGRESS"},
	})
	want := map[string]any{"failed": []string{"lint"}, "pending": 1, "passed": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tallyPRChecks() = %#v, want %#v", got, want)
	}
}

func TestClipCountsUnicodeCharacters(t *testing.T) {
	input := "界"
	for i := 0; i < bodyLimit; i++ {
		input += "界"
	}
	got := clip(input)
	if len([]rune(got)) != bodyLimit+1 || []rune(got)[bodyLimit] != '…' {
		t.Fatalf("clip returned %d runes; want %d including ellipsis", len([]rune(got)), bodyLimit+1)
	}
}

func TestJSONTextMatchesJavaScriptUnicodeEncoding(t *testing.T) {
	got, err := jsonText([]any{"<herdr>", "🌱  "}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := `["<herdr>","🌱  "]`
	if string(got) != want {
		t.Fatalf("jsonText() = %q, want %q", got, want)
	}
}
