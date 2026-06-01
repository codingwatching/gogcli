package cmd

import "testing"

func TestBestEffortWebURLExplicitTypeRejectsUnsupportedURLs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind string
		in   string
		want string
	}{
		{
			name: "non-google docs url",
			kind: "docs",
			in:   "https://example.com/not-a-google-id",
			want: "",
		},
		{
			name: "non-google gmail thread url",
			kind: "gmail-thread",
			in:   "https://example.com/not-gmail/18c0abc123def456",
			want: "",
		},
		{
			name: "raw docs id",
			kind: "docs",
			in:   "DOCID",
			want: "https://docs.google.com/document/d/DOCID/edit",
		},
		{
			name: "google docs url",
			kind: "docs",
			in:   "https://docs.google.com/document/d/DOCID/edit",
			want: "https://docs.google.com/document/d/DOCID/edit",
		},
		{
			name: "gmail thread id",
			kind: "gmail-thread",
			in:   "18c0abc123def456",
			want: "https://mail.google.com/mail/u/0/#all/18c0abc123def456",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := bestEffortWebURL(tt.kind, tt.in); got != tt.want {
				t.Fatalf("bestEffortWebURL(%q, %q) = %q, want %q", tt.kind, tt.in, got, tt.want)
			}
		})
	}
}
