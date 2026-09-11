package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleRegistrationEmail() transactionalEmail {
	return sampleRegistrationEmailIn(localeEN)
}

func sampleRegistrationEmailIn(locale mailLocale) transactionalEmail {
	c := copyFor(locale)
	return transactionalEmail{
		Locale:    locale,
		Greeting:  c.greetingPlain,
		Intro:     c.introRegister,
		CodeLabel: c.labelCode,
		Code:      "418062",
		CodeStyle: codeStyleDigits,
		Expiry:    expiryLine(locale, time.Date(2026, 9, 10, 14, 30, 0, 0, time.UTC), 15*time.Minute),
		Reassure:  c.reassureIgnore,
	}
}

func sampleResetEmail() transactionalEmail {
	c := copyFor(localeEN)
	return transactionalEmail{
		Locale:    localeEN,
		Greeting:  "Hello Ada,",
		Intro:     c.introReset,
		CodeLabel: c.labelToken,
		Code:      "9f2c1a7be40d5386c1ab77f0e9d4c2158b3a6de0c47f91b2a8e5d3c60f7148ab",
		CodeStyle: codeStyleToken,
		Expiry:    "This token expires at 2026-09-10T14:30:00Z UTC.",
		Reassure:  "If you did not request a reset you can ignore this email.",
	}
}

func TestRenderTransactionalEmailFillsEveryPlaceholder(t *testing.T) {
	for name, msg := range map[string]transactionalEmail{
		"registration": sampleRegistrationEmail(),
		"reset":        sampleResetEmail(),
	} {
		_, htmlBody := renderTransactionalEmail(msg)
		if strings.Contains(htmlBody, "__") {
			t.Fatalf("%s: template left an unreplaced placeholder:\n%s", name, htmlBody)
		}
		if !strings.Contains(htmlBody, msg.Code) {
			t.Fatalf("%s: rendered HTML does not contain the code", name)
		}
	}
}

// The retired internal codename must never reach a recipient.
func TestRenderTransactionalEmailCarriesNoRetiredCodename(t *testing.T) {
	for name, msg := range map[string]transactionalEmail{
		"registration": sampleRegistrationEmail(),
		"reset":        sampleResetEmail(),
	} {
		plain, htmlBody := renderTransactionalEmail(msg)
		for part, body := range map[string]string{"plain": plain, "html": htmlBody} {
			if strings.Contains(body, "XControl") {
				t.Fatalf("%s/%s still mentions XControl", name, part)
			}
			if !strings.Contains(body, brandProduct) {
				t.Fatalf("%s/%s does not mention %s", name, part, brandProduct)
			}
		}
	}
}

// English must stay ASCII: the mailer sends an all-ASCII plain part as 7bit,
// and a stray middot there would declare 7bit while shipping 8-bit bytes.
// Chinese is expected to be non-ASCII - the mailer switches that part to
// quoted-printable, which is covered in internal/mailer.
func TestRenderTransactionalEmailEnglishPlainPartIsASCII(t *testing.T) {
	plain, _ := renderTransactionalEmail(sampleRegistrationEmailIn(localeEN))
	for i := 0; i < len(plain); i++ {
		if plain[i] >= 0x80 {
			t.Fatalf("plain body byte %d is non-ASCII: %q", i, plain[i:])
		}
	}
}

func TestRenderTransactionalEmailChineseCarriesChineseCopy(t *testing.T) {
	plain, htmlBody := renderTransactionalEmail(sampleRegistrationEmailIn(localeZH))
	for name, body := range map[string]string{"plain": plain, "html": htmlBody} {
		if !strings.Contains(body, "验证码") {
			t.Fatalf("%s body is not in Chinese: %s", name, body)
		}
	}
	if !strings.Contains(htmlBody, `lang="zh-Hans"`) {
		t.Fatal("html document is not tagged as Chinese")
	}
}

func TestParseMailLocaleDefaultsToEnglish(t *testing.T) {
	for _, in := range []string{"", "fr-FR", "de", "  ", "xx;q=0.9"} {
		if got := parseMailLocale(in); got != localeEN {
			t.Fatalf("parseMailLocale(%q) = %q, want %q", in, got, localeEN)
		}
	}
	for _, in := range []string{"zh", "zh-CN", "zh-Hant,zh;q=0.9", "ZH-TW"} {
		if got := parseMailLocale(in); got != localeZH {
			t.Fatalf("parseMailLocale(%q) = %q, want %q", in, got, localeZH)
		}
	}
	// A browser that prefers French but accepts Chinese should still get
	// Chinese rather than the default, because Chinese is offered and French
	// is not.
	if got := parseMailLocale("fr-FR,fr;q=0.9,zh-CN;q=0.8"); got != localeZH {
		t.Fatalf("mixed header resolved to %q, want %q", got, localeZH)
	}
}

func TestRenderTransactionalEmailEscapesUntrustedText(t *testing.T) {
	msg := sampleRegistrationEmail()
	msg.Greeting = `Hello <script>alert("x")</script>,`
	_, htmlBody := renderTransactionalEmail(msg)
	if strings.Contains(htmlBody, "<script>") {
		t.Fatal("greeting was interpolated into the HTML body unescaped")
	}
}

// TestWriteEmailPreview renders both variants to disk for visual review, e.g.
//
//	EMAIL_PREVIEW_DIR=/tmp/preview go test ./api/ -run TestWriteEmailPreview
func TestWriteEmailPreview(t *testing.T) {
	dir := strings.TrimSpace(os.Getenv("EMAIL_PREVIEW_DIR"))
	if dir == "" {
		t.Skip("set EMAIL_PREVIEW_DIR to write preview files")
	}
	for name, msg := range map[string]transactionalEmail{
		"register-en": sampleRegistrationEmailIn(localeEN),
		"register-zh": sampleRegistrationEmailIn(localeZH),
		"reset":       sampleResetEmail(),
	} {
		plain, htmlBody := renderTransactionalEmail(msg)
		for ext, body := range map[string]string{"html": htmlBody, "txt": plain} {
			path := filepath.Join(dir, name+"."+ext)
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			t.Logf("wrote %s", path)
		}
	}
}
