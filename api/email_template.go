package api

import (
	"html"
	"strconv"
	"strings"
	"time"
)

// Brand strings for outbound transactional mail.
//
// The masthead is the product a recipient signed up for: XWorkmate, with the
// rest of the suite beneath it. svc.plus is the platform underneath - real, but
// infrastructure, and nobody scanning an inbox is looking for the name of the
// platform. It is credited once in the footer and nowhere else. XWork
// Technologies is the company that operates all of it.
//
// "XControl" is a retired internal codename and must never reach a recipient.
const (
	brandProduct  = "XWorkmate"
	brandSuite    = "XConnect · AI Workspace"
	brandPlatform = "svc.plus Platform"
	brandCompany  = "XWork Technologies"
	brandSiteURL  = "https://www.xworktech.com/"
	brandSiteText = "www.xworktech.com"
	// The English tagline the marketing site leads with. Keep the footer to
	// this one line: a verification mail is transactional, and bulk-sender
	// guidelines treat a promotional payload here as a deliverability risk.
	brandTagline = "One AI Workspace for all your AI."

	// The plain-text alternative is emitted with Content-Transfer-Encoding: 7bit,
	// so it must stay ASCII-only. Middots and other punctuation live in the HTML
	// part, which is quoted-printable encoded.
	brandSuiteASCII = "XConnect - AI Workspace"
)

// codeStyle selects how the highlighted credential is typeset. A six-digit
// verification code reads best tracked out on one line; a 64-character hex
// reset token has to wrap instead, or it overflows every mobile client.
type codeStyle int

const (
	codeStyleDigits codeStyle = iota
	codeStyleToken
)

// transactionalEmail is the content of one code-delivery message. Every field
// is plain text: the renderer escapes what goes into the HTML part.
type transactionalEmail struct {
	Greeting  string
	Intro     string
	CodeLabel string
	Code      string
	CodeStyle codeStyle
	Expiry    string
	Reassure  string
}

const emailFontStack = "-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,'Helvetica Neue',Arial,sans-serif"
const emailMonoStack = "ui-monospace,SFMono-Regular,Menlo,Consolas,'Liberation Mono',monospace"

// Table layout with inline styles throughout: Gmail strips <style> blocks and
// Outlook's word-based renderer ignores modern layout entirely.
const emailHTMLTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light">
<meta name="supported-color-schemes" content="light">
<title>__PRODUCT__</title>
</head>
<body style="margin:0;padding:0;background:#f4f5f7;">
<div style="display:none;font-size:1px;color:#f4f5f7;line-height:1px;max-height:0;max-width:0;opacity:0;overflow:hidden;">__PREHEADER__</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#f4f5f7;">
<tr><td align="center" style="padding:32px 16px;">
<table role="presentation" cellpadding="0" cellspacing="0" border="0" width="560" style="width:100%;max-width:560px;background:#ffffff;border:1px solid #e4e7ec;border-radius:12px;">
<tr><td style="padding:28px 32px 0 32px;">
<div style="margin:0;font-family:__FONT__;font-size:20px;font-weight:600;line-height:1.2;letter-spacing:-0.01em;color:#0f1729;">__PRODUCT__</div>
<div style="margin:6px 0 0 0;font-family:__FONT__;font-size:12px;font-weight:400;line-height:1.5;letter-spacing:0.02em;color:#667085;">__SUITE__</div>
</td></tr>
<tr><td style="padding:20px 32px 0 32px;"><div style="height:1px;line-height:1px;font-size:0;background:#e4e7ec;">&nbsp;</div></td></tr>
<tr><td style="padding:24px 32px 0 32px;font-family:__FONT__;font-size:15px;line-height:1.6;color:#344054;">
<p style="margin:0 0 12px 0;">__GREETING__</p>
<p style="margin:0;">__INTRO__</p>
</td></tr>
<tr><td style="padding:20px 32px 0 32px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">
<tr><td align="center" style="background:#f7f8fa;border:1px solid #e4e7ec;border-radius:10px;padding:20px 16px;">
<div style="margin:0 0 12px 0;font-family:__FONT__;font-size:11px;font-weight:500;line-height:1;letter-spacing:0.09em;text-transform:uppercase;color:#667085;">__CODE_LABEL__</div>
<div style="margin:0;font-family:__MONO__;__CODE_STYLE__color:#0f1729;">__CODE__</div>
</td></tr>
</table>
</td></tr>
<tr><td style="padding:16px 32px 0 32px;font-family:__FONT__;font-size:13px;line-height:1.6;color:#667085;">__EXPIRY__</td></tr>
<tr><td style="padding:12px 32px 24px 32px;font-family:__FONT__;font-size:13px;line-height:1.6;color:#667085;">__REASSURE__</td></tr>
<tr><td style="padding:0 32px 28px 32px;">
<div style="height:1px;line-height:1px;font-size:0;background:#e4e7ec;">&nbsp;</div>
<div style="margin:16px 0 0 0;font-family:__FONT__;font-size:13px;font-weight:500;line-height:1.5;color:#475467;">__TAGLINE__</div>
<div style="margin:4px 0 0 0;font-family:__FONT__;font-size:13px;line-height:1.5;"><a href="__SITE__" style="color:#3538cd;font-weight:600;text-decoration:none;">__SITE_TEXT__</a></div>
<div style="margin:14px 0 0 0;font-family:__FONT__;font-size:12px;line-height:1.6;color:#98a2b3;">
<a href="__SITE__" style="color:#667085;font-weight:600;text-decoration:none;">__COMPANY__</a><span style="color:#d0d5dd;">&nbsp;&middot;&nbsp;</span>__PLATFORM__
</div>
<div style="margin:4px 0 0 0;font-family:__FONT__;font-size:12px;line-height:1.6;color:#98a2b3;">This is an automated message, please do not reply.</div>
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`

// renderTransactionalEmail returns the plain-text and HTML alternatives for one
// message. Both carry the same information; the plain part stays ASCII-only.
func renderTransactionalEmail(m transactionalEmail) (string, string) {
	var codeCSS string
	switch m.CodeStyle {
	case codeStyleToken:
		// A 64-character token must break, and tracking would make it unreadable.
		codeCSS = "font-size:15px;font-weight:600;line-height:1.6;word-break:break-all;"
	default:
		codeCSS = "font-size:32px;font-weight:700;line-height:1.2;letter-spacing:0.28em;text-indent:0.28em;"
	}

	replacer := strings.NewReplacer(
		"__PRODUCT__", brandProduct,
		"__PLATFORM__", brandPlatform,
		"__SUITE__", brandSuite,
		"__COMPANY__", brandCompany,
		"__SITE__", brandSiteURL,
		"__SITE_TEXT__", brandSiteText,
		"__TAGLINE__", brandTagline,
		"__FONT__", emailFontStack,
		"__MONO__", emailMonoStack,
		"__CODE_STYLE__", codeCSS,
		"__PREHEADER__", html.EscapeString(m.CodeLabel+": "+m.Code),
		"__GREETING__", html.EscapeString(m.Greeting),
		"__INTRO__", html.EscapeString(m.Intro),
		"__CODE_LABEL__", html.EscapeString(m.CodeLabel),
		"__CODE__", html.EscapeString(m.Code),
		"__EXPIRY__", html.EscapeString(m.Expiry),
		"__REASSURE__", html.EscapeString(m.Reassure),
	)

	var plain strings.Builder
	plain.WriteString(brandProduct + " - " + brandSuiteASCII + "\n")
	plain.WriteString(strings.Repeat("-", 40) + "\n\n")
	plain.WriteString(m.Greeting + "\n\n")
	plain.WriteString(m.Intro + "\n\n")
	plain.WriteString(m.CodeLabel + ": " + m.Code + "\n\n")
	plain.WriteString(m.Expiry + "\n")
	plain.WriteString(m.Reassure + "\n\n")
	plain.WriteString(strings.Repeat("-", 40) + "\n")
	plain.WriteString(brandTagline + "\n")
	plain.WriteString(brandSiteURL + "\n\n")
	plain.WriteString(brandCompany + " - " + brandPlatform + "\n")
	plain.WriteString("This is an automated message, please do not reply.\n")

	return plain.String(), replacer.Replace(emailHTMLTemplate)
}

// expiryLine renders the shared "expires at ... (in N minutes)" sentence so the
// three transactional mails cannot drift apart in wording.
func expiryLine(expiresAt time.Time, ttl time.Duration) string {
	line := "This code expires at " + expiresAt.UTC().Format(time.RFC3339) + " UTC"
	if minutes := int(ttl.Minutes()); minutes > 0 {
		line += " (in " + strconv.Itoa(minutes) + " minutes)"
	}
	return line + "."
}
