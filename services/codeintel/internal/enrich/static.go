// Package enrich assigns criticality to inventory items: deterministic static rules always, and an optional
// Claude pass (structured JSON) that adds business domain, summary, failure modes, monitor templates and a
// load test hint. alerting.md § Kod-farkındalıklı otomatik alert.
package enrich

import (
	"regexp"
	"strings"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
	"github.com/tasktrooper/telemetry/services/codeintel/internal/route"
)

var (
	reCriticalWords = regexp.MustCompile(`pay|checkout|charge|billing|invoice|refund|order|auth|login|logout|signin|sign-in|signup|sign-up|register|password|token|oauth|session|subscription|wallet|transfer|ledger|settle|capture|purchase|cart`)
	reMoneyWords    = regexp.MustCompile(`pay|checkout|charge|billing|invoice|refund|ledger|settle|capture|purchase|subscription|wallet|transfer`)
	reTrivial       = regexp.MustCompile(`(^|/)(healthz?|health-check|livez?|readyz?|ready|metrics|ping|status|favicon\.ico|robots\.txt|version|static|assets|_next)(/|$)`)
	reDLQ           = regexp.MustCompile(`dlq|dead[-_.]?letter|deadletter|\.dead|retry`)
	reDBWrite       = regexp.MustCompile(`(?i)\b(insert\s+into|update\s+\w+\s+set|delete\s+from|begintx|begin\(|\.create\(|\.save\(|\.update\(|\.insert|\.delete\(|transaction|\.exec\(|\.commit\()`)
	rePaymentHosts  = regexp.MustCompile(`stripe|iyzi|paypal|adyen|braintree|checkout\.com|klarna|mollie|square`)
	reGRPCRead      = regexp.MustCompile(`^(get|list|watch|find|search|describe|check|count|stream|health)`)
)

// Static sets it.Criticality and it.Rationale from the item alone.
func Static(it *model.Item) {
	c, why := staticLevel(it)
	it.Criticality, it.Rationale = c, why
}

func staticLevel(it *model.Item) (string, string) {
	text := strings.ToLower(it.Key + " " + it.Handler + " " + it.Source)
	critical := reCriticalWords.FindString(text)
	switch it.Kind {
	case model.KindEndpoint:
		method, p := route.SplitKey(it.Key)
		lp := strings.ToLower(p)
		if reTrivial.MatchString(lp) {
			return model.Low, "health/infra endpoint"
		}
		write := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
		switch method {
		case "GRPC":
			m := strings.ToLower(lp[strings.LastIndexByte(lp, '/')+1:])
			write = !reGRPCRead.MatchString(m)
		case "ANY":
			write = reDBWrite.MatchString(it.Snippet)
		}
		switch {
		case critical != "" && write:
			return model.Critical, "write path on a critical business flow (" + critical + ")"
		case critical != "":
			return model.High, "read path on a critical business flow (" + critical + ")"
		case write:
			return model.Medium, "state-changing endpoint"
		}
		return model.Low, "read-only endpoint"
	case model.KindConsumer:
		switch {
		case reDLQ.MatchString(text):
			return model.High, "dead-letter / retry consumer"
		case critical != "":
			return model.High, "consumer on a critical business topic (" + critical + ")"
		}
		return model.Medium, "message consumer"
	case model.KindCron:
		if critical != "" {
			return model.High, "scheduled job on a critical business flow (" + critical + ")"
		}
		return model.Medium, "scheduled job"
	case model.KindView:
		lp := strings.ToLower(it.Key)
		switch {
		case reMoneyWords.MatchString(lp) || strings.Contains(lp, "cart") || strings.Contains(lp, "login") || strings.Contains(lp, "signup"):
			return model.High, "conversion-critical page"
		case lp == "/":
			return model.Medium, "landing page"
		}
		return model.Low, "page"
	case model.KindDependency:
		switch {
		case rePaymentHosts.MatchString(text):
			return model.High, "payment provider"
		case strings.HasPrefix(it.Key, "db ·"), strings.HasPrefix(it.Key, "queue ·"):
			return model.Medium, "data store / broker"
		}
		return model.Low, "external dependency"
	}
	return model.Low, ""
}

// BumpForTraffic raises medium items with heavy observed traffic (≥ 100 rpm) to high.
func BumpForTraffic(it *model.Item) {
	if it.Stats != nil && it.Stats.RPM >= 100 && it.Criticality == model.Medium {
		it.Criticality = model.High
		it.Rationale += "; high traffic"
	}
}
