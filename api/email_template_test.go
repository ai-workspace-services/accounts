package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleRegistrationEmail() transactionalEmail {
	return transactionalEmail{
		Greeting:  "Hello,",
		Intro:     "Use the verification code below to finish creating your " + brandProduct + " account.",
		CodeLabel: "Verification code",
		Code:      "418062",
		CodeStyle: codeStyleDigits,
		Expiry:    expiryLine(time.Date(2026, 9, 10, 14, 30, 0, 0, time.UTC), 15*time.Minute),
		Reassure:  "If you did not request this email you can ignore it.",
	}
}

func sampleResetEmail() transactionalEmail {
	return transactionalEmail{
		Greeting:  "Hello Ada,",
		Intro:     "Use the token below to reset your " + brandProduct + " account password.",
		CodeLabel: "Reset token",
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

// The plain-text alternative is sent as Content-Transfer-Encoding: 7bit, so any
// non-ASCII byte in it is a protocol violation some relays reject outright.
func TestRenderTransactionalEmailPlainPartIsASCII(t *testing.T) {
	plain, _ := renderTransactionalEmail(sampleRegistrationEmail())
	for i := 0; i < len(plain); i++ {
		if plain[i] >= 0x80 {
			t.Fatalf("plain body byte %d is non-ASCII: %q", i, plain[i:])
		}
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
		"register": sampleRegistrationEmail(),
		"reset":    sampleResetEmail(),
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
