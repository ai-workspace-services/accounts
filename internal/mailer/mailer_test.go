package mailer

import (
	"strings"
	"testing"
)

func TestParseTLSMode(t *testing.T) {
	cases := map[string]TLSMode{
		"":          TLSModeAuto,
		"auto":      TLSModeAuto,
		"automatic": TLSModeAuto,
		"detect":    TLSModeAuto,
		"starttls":  TLSModeStartTLS,
		"start_tls": TLSModeStartTLS,
		"start-tls": TLSModeStartTLS,
		"implicit":  TLSModeImplicit,
		"smtps":     TLSModeImplicit,
		"none":      TLSModeNone,
		"disable":   TLSModeNone,
		"disabled":  TLSModeNone,
		"off":       TLSModeNone,
		"plain":     TLSModeNone,
		"plaintext": TLSModeNone,
		"unknown":   TLSModeAuto,
	}

	for input, expected := range cases {
		if mode := ParseTLSMode(input); mode != expected {
			t.Errorf("ParseTLSMode(%q) = %q, expected %q", input, mode, expected)
		}
	}
}

func TestNewImplicitModeAutodetect(t *testing.T) {
	sender, err := New(Config{
		Host: "smtp.example.com",
		Port: 465,
		From: "Example <no-reply@example.com>",
	})
	if err != nil {
		t.Fatalf("unexpected error creating sender: %v", err)
	}

	s, ok := sender.(*smtpSender)
	if !ok {
		t.Fatalf("expected smtpSender, got %T", sender)
	}
	if s.tlsMode != TLSModeImplicit {
		t.Fatalf("expected tlsMode %q, got %q", TLSModeImplicit, s.tlsMode)
	}
}

// A Chinese notification must not be declared 7bit: that is a promise every
// byte is under 128, and relays are entitled to reject the message or hand on
// mojibake when it is broken.
func TestBuildMessageEncodesNonASCIIPlainPart(t *testing.T) {
	sender, err := New(Config{Host: "smtp.example.com", From: "a@example.com"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := sender.(*smtpSender)

	ascii, err := s.buildMessage(Message{PlainBody: "code 418062"}, []string{"b@example.com"})
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	if !strings.Contains(string(ascii), "Content-Transfer-Encoding: 7bit") {
		t.Fatal("an all-ASCII plain part should still be sent as 7bit")
	}

	chinese, err := s.buildMessage(Message{PlainBody: "验证码 418062"}, []string{"b@example.com"})
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	body := string(chinese)
	if strings.Contains(body, "Content-Transfer-Encoding: 7bit") {
		t.Fatalf("a Chinese plain part was declared 7bit:\n%s", body)
	}
	if !strings.Contains(body, "Content-Transfer-Encoding: quoted-printable") {
		t.Fatalf("a Chinese plain part was not quoted-printable:\n%s", body)
	}
}
