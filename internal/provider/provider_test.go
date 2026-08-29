package provider

import "testing"

func TestDisplayName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"OUTLOOK", "Outlook"},
		{"WHATSAPP", "WhatsApp"},
		{"", ""},
		{"FAKECHAT", "Fakechat"},
		{"gmail", "Gmail"},
		{"X", "X"},
	}
	for _, c := range cases {
		if got := DisplayName(c.in); got != c.want {
			t.Errorf("DisplayName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
