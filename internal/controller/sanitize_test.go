package controller

import (
	"errors"
	"testing"
)

func TestSanitizeUserVisibleStripsURLUserinfoAndQuery(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			in:   "GET https://user:secret@host/path?token=abc: connection refused",
			want: "GET https://host/path: connection refused",
		},
		{
			in:   "https://user:secret@host/path?token=abc",
			want: "https://host/path",
		},
		{
			in:   "https://host/path?token=abc",
			want: "https://host/path",
		},
		{
			in:   "https://user@host/path",
			want: "https://host/path",
		},
		{
			in:   "http://alice:pw@example.com:8443/v1/obj?sig=deadbeef",
			want: "http://example.com:8443/v1/obj",
		},
		{
			in:   "AccessDenied: not authorized to perform DeleteObject",
			want: "AccessDenied: not authorized to perform DeleteObject",
		},
		{
			in:   "observe: GET https://user:secret@host/path?token=abc: EOF (retry)",
			want: "observe: GET https://host/path: EOF (retry)",
		},
	}
	for _, tc := range cases {
		got := sanitizeUserVisible(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeUserVisible(%q)\n  got  %q\n  want %q", tc.in, got, tc.want)
		}
	}
}

func TestUserVisibleErrorNil(t *testing.T) {
	if got := userVisibleError(nil); got != "" {
		t.Fatalf("userVisibleError(nil) = %q, want empty", got)
	}
	err := errors.New("GET https://user:secret@host/path?token=abc: boom")
	got := userVisibleError(err)
	if got != "GET https://host/path: boom" {
		t.Fatalf("userVisibleError = %q", got)
	}
	if err.Error() == got {
		t.Fatal("unsanitized error was mutated")
	}
}
