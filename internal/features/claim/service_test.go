package claim

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestValidateLimits(t *testing.T) {
	cases := []struct {
		name    string
		in      *ClaimInput
		wantErr bool
	}{
		{
			name:    "empty payload ok",
			in:      &ClaimInput{ClientDedupeKey: uuid.New()},
			wantErr: false,
		},
		{
			name: "snippets at cap ok",
			in: &ClaimInput{
				ClientDedupeKey: uuid.New(),
				Snippets:        make([]SnippetInput, MaxSnippets),
			},
			wantErr: false,
		},
		{
			name: "snippets over cap rejected",
			in: &ClaimInput{
				ClientDedupeKey: uuid.New(),
				Snippets:        make([]SnippetInput, MaxSnippets+1),
			},
			wantErr: true,
		},
		{
			name: "elements aggregate over cap rejected",
			in: &ClaimInput{
				ClientDedupeKey: uuid.New(),
				Snippets: []SnippetInput{
					{Elements: make([]ElementInput, MaxElements+1)},
				},
			},
			wantErr: true,
		},
		{
			name: "pages over cap rejected",
			in: &ClaimInput{
				ClientDedupeKey: uuid.New(),
				Pages:           make([]PageInput, MaxPages+1),
			},
			wantErr: true,
		},
		{
			name: "tags over cap rejected",
			in: &ClaimInput{
				ClientDedupeKey: uuid.New(),
				Tags:            make([]TagInput, MaxTags+1),
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLimits(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestStringBuilderWriteIntPlus(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{9, "9"},
		{10, "10"},
		{42, "42"},
		{1234, "1234"},
		{1000000, "1000000"},
	}
	for _, tc := range cases {
		var sb stringBuilder
		sb.WriteIntPlus(tc.in)
		if got := sb.String(); got != tc.want {
			t.Errorf("WriteIntPlus(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStringBuilderCombo(t *testing.T) {
	var sb stringBuilder
	sb.WriteString("INSERT INTO tags VALUES ($")
	sb.WriteIntPlus(1)
	sb.WriteString(",$")
	sb.WriteIntPlus(2)
	sb.WriteString(")")
	got := sb.String()
	want := "INSERT INTO tags VALUES ($1,$2)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !strings.Contains(got, "$1") || !strings.Contains(got, "$2") {
		t.Fatalf("placeholder verification failed: %q", got)
	}
}
