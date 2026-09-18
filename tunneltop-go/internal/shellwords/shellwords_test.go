package shellwords

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "simple",
			in:   "ssh -N -L 8080:127.0.0.1:80 user@example.com",
			want: []string{"ssh", "-N", "-L", "8080:127.0.0.1:80", "user@example.com"},
		},
		{
			name: "single quoted",
			in:   "sh -c 'echo hello world'",
			want: []string{"sh", "-c", "echo hello world"},
		},
		{
			name: "double quoted curl format",
			in:   `curl -s -w "%{http_code}" https://example.com`,
			want: []string{"curl", "-s", "-w", `%{http_code}`, "https://example.com"},
		},
		{
			name: "empty quoted arg",
			in:   `cmd "" '' x`,
			want: []string{"cmd", "", "", "x"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Split(tt.in)
			if err != nil {
				t.Fatalf("Split() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Split() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestSplitErrors(t *testing.T) {
	for _, in := range []string{`echo "unterminated`, `echo 'unterminated`, `echo \`} {
		if _, err := Split(in); err == nil {
			t.Fatalf("Split(%q) expected error", in)
		}
	}
}
