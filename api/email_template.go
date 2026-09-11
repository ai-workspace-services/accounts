package api

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Brand strings for outbound transactional mail.
//
// The masthead is the product a recipient signed up for: XWorkmate, with a line
// saying what it is for beneath it - a sentence a stranger can read, rather than
// a list of service names that only means something to someone already inside. svc.plus is the platform underneath - real, but
// infrastructure, and nobody scanning an inbox is looking for the name of the
// platform. It is credited once in the footer and nowhere else. XWork
// Technologies is the company that operates all of it.
//
// "XControl" is a retired internal codename and must never reach a recipient.
const (
	brandProduct  = "XWorkmate"
	brandPlatform = "svc.plus Platform"
	brandCompany  = "XWork Technologies"
	brandSiteURL  = "https://www.xworktech.com/"
	brandSiteText = "www.xworktech.com"
)

// mailLocale selects the language of a transactional mail. English is the
// default because it is the only language every recipient of this product has
// in common; Chinese is offered because most of them read it more comfortably.
type mailLocale string

const (
	localeEN mailLocale = "en"
	localeZH mailLocale = "zh"
)

// mailCopy is every sentence that changes with the language. Brand names do
// not appear here on purpose: XWorkmate and svc.plus Platform are the same
// words in both, and duplicating them would be one more place to forget.
type mailCopy struct {
	htmlLang        string
	suite           string
	greetingPlain   string // no recipient name is known
	greetingNamed   string // %s is the recipient's name
	introRegister   string
	introVerify     string
	introReset      string
	labelCode       string
	labelToken      string
	expiry          string // %s is an RFC3339 UTC timestamp
	expiryMinutes   string // %s timestamp, %d minutes
	reassureIgnore  string
	reassureReset   string
	subjectRegister string
	subjectVerify   string
	subjectReset    string
	automatedNotice string
}

var mailCopyByLocale = map[mailLocale]mailCopy{
	localeEN: {
		htmlLang:        "en",
		suite:           "Connect and work with your AI Workspace",
		greetingPlain:   "Hello,",
		greetingNamed:   "Hello %s,",
		introRegister:   "Use the verification code below to finish creating your account.",
		introVerify:     "Use the verification code below to verify your account.",
		introReset:      "Use the token below to reset your account password.",
		labelCode:       "Verification code",
		labelToken:      "Reset token",
		expiry:          "This token expires at %s UTC.",
		expiryMinutes:   "This code expires at %s UTC (in %d minutes).",
		reassureIgnore:  "If you did not request this email you can ignore it.",
		reassureReset:   "If you did not request a reset you can ignore this email.",
		subjectRegister: "Verify your email for " + brandProduct,
		subjectVerify:   "Verify your " + brandProduct + " account",
		subjectReset:    "Reset your " + brandProduct + " password",
		automatedNotice: "This is an automated message, please do not reply.",
	},
	localeZH: {
		htmlLang:        "zh-Hans",
		suite:           "连接并驾驭你的 AI 工作空间",
		greetingPlain:   "您好：",
		greetingNamed:   "%s，您好：",
		introRegister:   "请使用下方验证码完成账号创建。",
		introVerify:     "请使用下方验证码完成邮箱验证。",
		introReset:      "请使用下方令牌重置账号密码。",
		labelCode:       "验证码",
		labelToken:      "重置令牌",
		expiry:          "该令牌于 %s UTC 失效。",
		expiryMinutes:   "该验证码于 %s UTC 失效（%d 分钟后）。",
		reassureIgnore:  "如果这不是你发起的请求，忽略本邮件即可。",
		reassureReset:   "如果这不是你发起的重置请求，忽略本邮件即可。",
		subjectRegister: brandProduct + " 邮箱验证码",
		subjectVerify:   "验证你的 " + brandProduct + " 账号",
		subjectReset:    "重置你的 " + brandProduct + " 密码",
		automatedNotice: "本邮件由系统自动发送，请勿直接回复。",
	},
}

// copyFor never fails: an unknown locale falls back to English rather than
// rendering a mail with empty sentences in it.
func copyFor(locale mailLocale) mailCopy {
	if c, ok := mailCopyByLocale[locale]; ok {
		return c
	}
	return mailCopyByLocale[localeEN]
}

// parseMailLocale resolves a locale from whatever the caller knows, which is
// usually an Accept-Language header. Only the primary subtag is inspected, so
// zh-CN, zh-TW and zh all land on Chinese; everything else is English.
func parseMailLocale(value string) mailLocale {
	for _, part := range strings.Split(value, ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		switch {
		case tag == "":
			continue
		case strings.HasPrefix(tag, "zh"):
			return localeZH
		case strings.HasPrefix(tag, "en"):
			return localeEN
		}
	}
	return localeEN
}

// requestLocale picks the language for a mail sent in response to this request.
//
// An explicit ?locale= or X-Locale wins, because the console knows which
// language the person is actually reading; Accept-Language is the fallback the
// browser supplies on its own. Neither is trusted to be valid - parseMailLocale
// resolves anything it does not recognise to English.
func requestLocale(c *gin.Context) mailLocale {
	if c == nil || c.Request == nil {
		return localeEN
	}
	for _, explicit := range []string{c.Query("locale"), c.GetHeader("X-Locale")} {
		if strings.TrimSpace(explicit) != "" {
			return parseMailLocale(explicit)
		}
	}
	return parseMailLocale(c.GetHeader("Accept-Language"))
}

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
	Locale    mailLocale
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
<html lang="__LANG__">
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
<div style="margin:16px 0 0 0;font-family:__FONT__;font-size:13px;line-height:1.5;"><a href="__SITE__" style="color:#3538cd;font-weight:600;text-decoration:none;">__SITE_TEXT__</a></div>
<div style="margin:14px 0 0 0;font-family:__FONT__;font-size:12px;line-height:1.6;color:#98a2b3;">
<a href="__SITE__" style="color:#667085;font-weight:600;text-decoration:none;">__COMPANY__</a><span style="color:#d0d5dd;">&nbsp;&middot;&nbsp;</span>__PLATFORM__
</div>
<div style="margin:4px 0 0 0;font-family:__FONT__;font-size:12px;line-height:1.6;color:#98a2b3;">__NOTICE__</div>
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`

// renderTransactionalEmail returns the plain-text and HTML alternatives for one
// message. Both carry the same information; the plain part stays ASCII-only.
func renderTransactionalEmail(m transactionalEmail) (string, string) {
	c := copyFor(m.Locale)

	var codeCSS string
	switch m.CodeStyle {
	case codeStyleToken:
		// A 64-character token must break, and tracking would make it unreadable.
		codeCSS = "font-size:15px;font-weight:600;line-height:1.6;word-break:break-all;"
	default:
		codeCSS = "font-size:32px;font-weight:700;line-height:1.2;letter-spacing:0.28em;text-indent:0.28em;"
	}

	replacer := strings.NewReplacer(
		"__LANG__", c.htmlLang,
		"__PRODUCT__", brandProduct,
		"__PLATFORM__", brandPlatform,
		"__NOTICE__", html.EscapeString(c.automatedNotice),
		"__SUITE__", c.suite,
		"__COMPANY__", brandCompany,
		"__SITE__", brandSiteURL,
		"__SITE_TEXT__", brandSiteText,
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
	plain.WriteString(brandProduct + " - " + c.suite + "\n")
	plain.WriteString(strings.Repeat("-", 40) + "\n\n")
	plain.WriteString(m.Greeting + "\n\n")
	plain.WriteString(m.Intro + "\n\n")
	plain.WriteString(m.CodeLabel + ": " + m.Code + "\n\n")
	plain.WriteString(m.Expiry + "\n")
	plain.WriteString(m.Reassure + "\n\n")
	plain.WriteString(strings.Repeat("-", 40) + "\n")
	plain.WriteString(brandSiteURL + "\n\n")
	plain.WriteString(brandCompany + " - " + brandPlatform + "\n")
	plain.WriteString(c.automatedNotice + "\n")

	return plain.String(), replacer.Replace(emailHTMLTemplate)
}

// expiryLine renders the shared "expires at ... (in N minutes)" sentence so the
// three transactional mails cannot drift apart in wording.
func expiryLine(locale mailLocale, expiresAt time.Time, ttl time.Duration) string {
	c := copyFor(locale)
	stamp := expiresAt.UTC().Format(time.RFC3339)
	if minutes := int(ttl.Minutes()); minutes > 0 {
		return fmt.Sprintf(c.expiryMinutes, stamp, minutes)
	}
	return fmt.Sprintf(c.expiry, stamp)
}
