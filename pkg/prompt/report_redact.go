package prompt

import (
	"regexp"
	"strings"

	"github.com/yuuki/grafana-llm-studio-app/pkg/ir"
)

var (
	reAuthHeader   = regexp.MustCompile(`(?i)\b(authorization\s*:\s*(?:bearer|basic)\s+|cookie\s*:\s*)[^\s;,\]"']+`)
	reBearerInline = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{12,}`)
	reGrafanaToken = regexp.MustCompile(`\bgl(?:sa|c)_[A-Za-z0-9_-]{16,}\b`)
	reGitHubToken  = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)
	reSlackToken   = regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{16,}\b`)
	reAWSAccessKey = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	reOpenAIKey    = regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}\b`)
	reJWT          = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	rePEMBlock     = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	rePEMHeader    = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	reCredURI      = regexp.MustCompile(`(?i)\b(?:https?|postgres(?:ql)?|rediss?|mysql|mongodb(?:\+srv)?|amqps?)://[^:\s/@"'<>]+:[^@\s/"'<>]+@[^\s"'<>]+`)
	reQuerySecret  = regexp.MustCompile(`(?i)([?&](?:token|access_token|api_?key|secret|password|passwd|sig|x-amz-signature)=)[^&\s"'<>]+`)
	reJSONSecret   = regexp.MustCompile(`(?i)("(?:token|access_token|api_?key|password|passwd|client_secret|private_key)"\s*:\s*")[^"]+(")`)
	reKVSecret     = regexp.MustCompile(`(?i)\b((?:password|passwd|api_?key|access_key|client_secret|access_token|auth_token)\s*=\s*)[^\s,;'"&]+`)
	reHTTPURL      = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>()]+`)
)

// RedactSecrets replaces high-confidence credential/secret patterns in untrusted text.
// It intentionally avoids clobbering short Kubernetes Secret resource names in label maps
// (e.g. secret=tls-cert-prod) while redacting tokens, headers, DSNs, PEM keys, and query secrets.
func RedactSecrets(in string) string {
	if in == "" {
		return ""
	}
	out := in
	out = rePEMBlock.ReplaceAllString(out, "[REDACTED_KEY]")
	out = rePEMHeader.ReplaceAllString(out, "[REDACTED_KEY]")
	out = reCredURI.ReplaceAllString(out, "[REDACTED_URI]")
	out = reAuthHeader.ReplaceAllString(out, "${1}[REDACTED]")
	out = reBearerInline.ReplaceAllString(out, "${1} [REDACTED]")
	out = reGrafanaToken.ReplaceAllString(out, "[REDACTED]")
	out = reGitHubToken.ReplaceAllString(out, "[REDACTED]")
	out = reSlackToken.ReplaceAllString(out, "[REDACTED]")
	out = reAWSAccessKey.ReplaceAllString(out, "[REDACTED]")
	out = reOpenAIKey.ReplaceAllString(out, "[REDACTED]")
	out = reJWT.ReplaceAllString(out, "[REDACTED]")
	out = reQuerySecret.ReplaceAllString(out, "${1}[REDACTED]")
	out = reJSONSecret.ReplaceAllString(out, "${1}[REDACTED]${2}")
	out = reKVSecret.ReplaceAllString(out, "${1}[REDACTED]")
	return out
}

// RedactSecretsAndURLs applies secret redaction and scrubs http(s) URLs to [URL]
// so upstream alert/error messages cannot leak URLs into LLM prompts and cause
// fail-closed URL rejections on report output.
func RedactSecretsAndURLs(in string) string {
	if in == "" {
		return ""
	}
	out := RedactSecrets(in)
	out = reHTTPURL.ReplaceAllString(out, "[URL]")
	return strings.TrimSpace(out)
}

// RedactStatusReport applies secret redaction to all prose fields of a validated StatusReportIR
// before persisting it in JobStore (defense-in-depth against LLM echoing secrets).
func RedactStatusReport(report *ir.StatusReportIR) {
	if report == nil {
		return
	}
	report.ExecutiveSummary = RedactSecrets(report.ExecutiveSummary)
	report.SuggestedInvestigationIntent = RedactSecrets(report.SuggestedInvestigationIntent)
	for i := range report.Timeline {
		report.Timeline[i].Timestamp = RedactSecrets(report.Timeline[i].Timestamp)
		report.Timeline[i].Event = RedactSecrets(report.Timeline[i].Event)
	}
	for i := range report.Findings {
		report.Findings[i].Title = RedactSecrets(report.Findings[i].Title)
		report.Findings[i].Detail = RedactSecrets(report.Findings[i].Detail)
	}
	for i := range report.Hypotheses {
		report.Hypotheses[i].Title = RedactSecrets(report.Hypotheses[i].Title)
		report.Hypotheses[i].Rationale = RedactSecrets(report.Hypotheses[i].Rationale)
		for j := range report.Hypotheses[i].VerificationSteps {
			report.Hypotheses[i].VerificationSteps[j] = RedactSecrets(report.Hypotheses[i].VerificationSteps[j])
		}
	}
	for i := range report.RecommendedActions {
		report.RecommendedActions[i].Action = RedactSecrets(report.RecommendedActions[i].Action)
	}
	for i := range report.DataGaps {
		report.DataGaps[i] = RedactSecrets(report.DataGaps[i])
	}
}
