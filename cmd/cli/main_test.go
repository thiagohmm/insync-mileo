package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSafeLocalName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "replace slashes",
			in:   "folder/file",
			want: "folder_file",
		},
		{
			name: "replace backslashes",
			in:   "folder\\file",
			want: "folder_file",
		},
		{
			name: "replace colons",
			in:   "file:with:colons",
			want: "file_with_colons",
		},
		{
			name: "multiple replacements",
			in:   "a/b\\c:d",
			want: "a_b_c_d",
		},
		{
			name: "trim spaces",
			in:   "  spaced  ",
			want: "spaced",
		},
		{
			name: "empty string defaults",
			in:   "",
			want: "sync-item",
		},
		{
			name: "dot defaults",
			in:   ".",
			want: "sync-item",
		},
		{
			name: "dotdot defaults",
			in:   "..",
			want: "sync-item",
		},
		{
			name: "normal name unchanged",
			in:   "normal-file.txt",
			want: "normal-file.txt",
		},
		{
			name: "only special chars",
			in:   "/\\:",
			want: "___",
		},
		{
			name: "only spaces defaults to sync-item",
			in:   "   ",
			want: "sync-item",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := safeLocalName(tt.in)
			if got != tt.want {
				t.Errorf("safeLocalName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDefaultSyncRoot(t *testing.T) {
	got := defaultSyncRoot()

	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		want := filepath.Join(home, "Insync")
		if got != want {
			t.Errorf("defaultSyncRoot() = %q, want %q", got, want)
		}
	}
}
