package plugin

import "testing"

func TestMaskEmail(t *testing.T) {
	for input, want := range map[string]string{
		"a@example.com":               "*@example.com",
		"ab@example.com":              "a*@example.com",
		"private@example.com":         "pr****e@example.com",
		"李明@example.com":              "李*@example.com",
		"contact private@example.com": "contact pr****e@example.com",
	} {
		if got := maskEmails(input); got != want {
			t.Errorf("maskEmails(%q) = %q, want %q", input, got, want)
		}
	}
}
