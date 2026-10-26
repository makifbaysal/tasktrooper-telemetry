package norm

import "strings"

// Caller kinds written to spans.caller_kind.
const (
	CallerService = "service"
	CallerWeb     = "web"
	CallerMobile  = "mobile"
	CallerProbe   = "probe"
	CallerWebhook = "webhook"
	CallerUnknown = "unknown"
)

var probeUA = []string{"kube-probe", "elb-healthchecker", "googlehc", "health", "uptimerobot", "pingdom", "consul", "blackbox-exporter", "datadog agent", "statuscake", "site24x7", "gke-l7"}

var webhookUA = []struct{ needle, name string }{
	{"stripe", "Stripe"},
	{"github-hookshot", "GitHub"},
	{"shopify", "Shopify"},
	{"slackbot", "Slack"},
	{"twilio", "Twilio"},
	{"paypal", "PayPal"},
	{"gitlab", "GitLab"},
	{"bitbucket", "Bitbucket"},
	{"sendgrid", "SendGrid"},
	{"mailgun", "Mailgun"},
	{"iyzico", "iyzico"},
}

var mobileUA = []string{"okhttp", "cfnetwork", "dart:io", "dart/", "alamofire", "darwin/", "dalvik", "react-native", "expo", "ktor-client"}

// ClassifyCaller maps an incoming request's user-agent to (caller, caller_kind) for entry spans
// that carry no trooper.caller.
func ClassifyCaller(ua string) (caller, kind string) {
	if ua == "" {
		return CallerUnknown, CallerUnknown
	}
	l := strings.ToLower(ua)
	for _, p := range probeUA {
		if strings.Contains(l, p) {
			return CallerProbe, CallerProbe
		}
	}
	for _, w := range webhookUA {
		if strings.Contains(l, w.needle) {
			return w.name, CallerWebhook
		}
	}
	for _, m := range mobileUA {
		if strings.Contains(l, m) {
			return CallerMobile, CallerMobile
		}
	}
	if strings.HasPrefix(l, "mozilla/") || strings.Contains(l, "applewebkit") || strings.Contains(l, "gecko/") {
		if strings.Contains(l, "bot") || strings.Contains(l, "spider") || strings.Contains(l, "crawl") {
			return CallerUnknown, CallerUnknown
		}
		return CallerWeb, CallerWeb
	}
	return CallerUnknown, CallerUnknown
}

// UA is the coarse result of a user-agent parse.
type UA struct {
	Browser, BrowserVersion string
	OS, OSVersion           string
	DeviceType              string // desktop | mobile | tablet | tv | bot | ""
}

// ParseUA extracts browser, OS and device type from a user-agent string. Cheap substring matching:
// good enough for RUM dimensions, not a full UA database.
func ParseUA(ua string) UA {
	var r UA
	if ua == "" {
		return r
	}
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "bot") || strings.Contains(l, "spider") || strings.Contains(l, "crawler") || strings.Contains(l, "headlesschrome"):
		r.DeviceType = "bot"
	case strings.Contains(l, "ipad") || strings.Contains(l, "tablet") || (strings.Contains(l, "android") && !strings.Contains(l, "mobile")):
		r.DeviceType = "tablet"
	case strings.Contains(l, "smart-tv") || strings.Contains(l, "smarttv") || strings.Contains(l, "appletv") || strings.Contains(l, "crkey"):
		r.DeviceType = "tv"
	case strings.Contains(l, "mobi") || strings.Contains(l, "iphone") || strings.Contains(l, "ipod"):
		r.DeviceType = "mobile"
	default:
		r.DeviceType = "desktop"
	}

	switch {
	case strings.Contains(l, "windows nt"):
		r.OS, r.OSVersion = "Windows", versionAfter(ua, "Windows NT ")
	case strings.Contains(l, "iphone") || strings.Contains(l, "ipad") || strings.Contains(l, "ipod"):
		r.OS, r.OSVersion = "iOS", strings.ReplaceAll(versionAfter(ua, "OS "), "_", ".")
	case strings.Contains(l, "android"):
		r.OS, r.OSVersion = "Android", versionAfter(ua, "Android ")
	case strings.Contains(l, "cros"):
		r.OS = "ChromeOS"
	case strings.Contains(l, "mac os x") || strings.Contains(l, "macintosh"):
		r.OS, r.OSVersion = "macOS", strings.ReplaceAll(versionAfter(ua, "Mac OS X "), "_", ".")
	case strings.Contains(l, "linux"):
		r.OS = "Linux"
	}

	switch {
	case strings.Contains(ua, "Edg/"), strings.Contains(ua, "EdgA/"), strings.Contains(ua, "EdgiOS/"):
		r.Browser, r.BrowserVersion = "Edge", firstVersion(ua, "Edg/", "EdgA/", "EdgiOS/")
	case strings.Contains(ua, "OPR/"):
		r.Browser, r.BrowserVersion = "Opera", versionAfter(ua, "OPR/")
	case strings.Contains(ua, "SamsungBrowser/"):
		r.Browser, r.BrowserVersion = "Samsung Internet", versionAfter(ua, "SamsungBrowser/")
	case strings.Contains(ua, "YaBrowser/"):
		r.Browser, r.BrowserVersion = "Yandex", versionAfter(ua, "YaBrowser/")
	case strings.Contains(ua, "Firefox/"), strings.Contains(ua, "FxiOS/"):
		r.Browser, r.BrowserVersion = "Firefox", firstVersion(ua, "Firefox/", "FxiOS/")
	case strings.Contains(ua, "CriOS/"):
		r.Browser, r.BrowserVersion = "Chrome", versionAfter(ua, "CriOS/")
	case strings.Contains(ua, "Chrome/"):
		r.Browser, r.BrowserVersion = "Chrome", versionAfter(ua, "Chrome/")
	case strings.Contains(ua, "Safari/") && strings.Contains(ua, "Version/"):
		r.Browser, r.BrowserVersion = "Safari", versionAfter(ua, "Version/")
	}
	return r
}

func firstVersion(ua string, keys ...string) string {
	for _, k := range keys {
		if v := versionAfter(ua, k); v != "" {
			return v
		}
	}
	return ""
}

// versionAfter returns the dotted/underscored version that follows key, major.minor only.
func versionAfter(ua, key string) string {
	i := strings.Index(ua, key)
	if i < 0 {
		return ""
	}
	s := ua[i+len(key):]
	end, dots := 0, 0
	for end < len(s) {
		c := s[end]
		if c == '.' || c == '_' {
			dots++
			if dots == 2 {
				break
			}
		} else if !isDigit(c) {
			break
		}
		end++
	}
	return strings.TrimRight(s[:end], "._")
}
