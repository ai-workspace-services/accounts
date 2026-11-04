package config

import (
	"gopkg.in/yaml.v3"
	"testing"
)

func TestSMTPDoesNotRequireRegistrationVerification(t *testing.T) {
	for _, tc := range []struct {
		name, registration string
		want               bool
	}{
		{"default", "", false},
		{"disabled", "registration:\n  emailVerification: false\n", false},
		{"enabled", "registration:\n  emailVerification: true\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte("smtp:\n  host: smtp.provider.test\n  port: 587\n  username: sender\n  password: test-only\n  from: sender@example.test\n"+tc.registration), &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Registration.EmailVerification != tc.want {
				t.Fatalf("registration verification=%v, want %v", cfg.Registration.EmailVerification, tc.want)
			}
			if cfg.SMTP.Host != "smtp.provider.test" {
				t.Fatal("SMTP configuration lost")
			}
		})
	}
}
