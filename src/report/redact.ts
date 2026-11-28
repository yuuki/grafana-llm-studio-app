import {
  AnalyzeStatusPayload,
  CollectionWarningItem,
  MAX_ANALYZE_FOCUS_RUNES,
  MAX_ANALYZE_PAYLOAD_BYTES,
} from '../ir/reportTypes';

export interface RedactionResult {
  text: string;
  count: number;
}

const SECRET_RULES: ReadonlyArray<{ re: RegExp; replacement: string }> = [
  {
    re: /-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----/g,
    replacement: '[REDACTED_KEY]',
  },
  {
    re: /-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?:(?:\r?\n|[ \t]+)[A-Za-z0-9+/=]{16,})*/g,
    replacement: '[REDACTED_KEY]',
  },
  {
    re: /\b(?:https?|postgres(?:ql)?|rediss?|mysql|mongodb(?:\+srv)?|amqps?):\/\/[^:\s/@"'<>]*:[^@\s/"'<>]+@[^\s"'<>]+/gi,
    replacement: '[REDACTED_URI]',
  },
  {
    re: /\b(cookie\s*:\s*)[^\r\n"']+/gi,
    replacement: '$1[REDACTED]',
  },
  {
    re: /\b(authorization\s*:\s*(?:(?:bearer|basic|token|apikey|digest)\s+)?|x-(?:api-key|auth-token)\s*:\s*)[^\s;,\]"']+/gi,
    replacement: '$1[REDACTED]',
  },
  {
    re: /\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{12,}/gi,
    replacement: '$1 [REDACTED]',
  },
  {
    re: /\bgl(?:sa|c)_[A-Za-z0-9_-]{16,}\b/g,
    replacement: '[REDACTED]',
  },
  {
    re: /\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b/g,
    replacement: '[REDACTED]',
  },
  {
    re: /\bxox[baprs]-[A-Za-z0-9-]{16,}\b/g,
    replacement: '[REDACTED]',
  },
  {
    re: /\bAKIA[0-9A-Z]{16}\b/g,
    replacement: '[REDACTED]',
  },
  {
    re: /\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}\b/g,
    replacement: '[REDACTED]',
  },
  {
    re: /\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b/g,
    replacement: '[REDACTED]',
  },
  {
    re: /([?&](?:token|access_token|api_?key|secret|password|passwd|sig|x-amz-signature)=)[^&\s"'<>]+/gi,
    replacement: '$1[REDACTED]',
  },
  {
    re: /("(?:token|access_token|api_?key|password|passwd|client_secret|private_key)"\s*:\s*")[^"]+(")/gi,
    replacement: '$1[REDACTED]$2',
  },
  {
    re: /\b((?:password|passwd|api_?key|access_key|client_secret|access_token|auth_token)\s*=\s*)(?:"[^"]*"|'[^']*'|[^\s,;'"&]+)/gi,
    replacement: '$1[REDACTED]',
  },
];

const HTTP_URL_RE = /\bhttps?:\/\/[^\s"'<>()]+/gi;

// Rune limits mirroring pkg/ir/report.go so client-side redaction expansion never triggers HTTP 400.
const MAX_TITLE_RUNES = 120;
const MAX_NAME_RUNES = 128;
const MAX_SEVERITY_RUNES = 32;
const MAX_LABEL_VAL_RUNES = 128;
const MAX_GROUP_RUNES = 100;
const MAX_SUMMARY_RUNES = 200;
const MAX_DESC_RUNES = 300;
const MAX_ERROR_RUNES = 240;
const MAX_SERIES_LABEL_RUNES = 140;
const MAX_UNIT_RUNES = 64;

function clampRunes(value: string, maxRunes: number): string {
  const chars = Array.from(value);
  if (chars.length <= maxRunes) {
    return value;
  }
  return chars.slice(0, maxRunes).join('').trim();
}

function nextWarnId(warnings: CollectionWarningItem[]): string {
  let maxNum = 0;
  for (const w of warnings) {
    const m = /^ev-warn-([1-9][0-9]{0,2})$/.exec(w.id);
    if (m) {
      const n = Number(m[1]);
      if (n > maxNum) {
        maxNum = n;
      }
    }
  }
  return `ev-warn-${maxNum + 1}`;
}

/**
 * Redacts high-confidence credential/secret patterns in untrusted telemetry text while
 * avoiding false positives on Kubernetes pod names or short Secret label values.
 */
export function redactSecrets(input: string): RedactionResult {
  if (!input) {
    return { text: '', count: 0 };
  }
  let out = input;
  let count = 0;
  for (const rule of SECRET_RULES) {
    rule.re.lastIndex = 0;
    const matches = out.match(rule.re);
    if (matches && matches.length > 0) {
      count += matches.length;
      out = out.replace(rule.re, rule.replacement);
    }
  }
  return { text: out, count };
}

/**
 * Redacts secrets and scrubs http(s) URLs to [URL] so upstream error/annotation URLs
 * cannot trigger fail-closed URL rejection on LLM output.
 */
export function redactSecretsAndURLs(input: string): RedactionResult {
  if (!input) {
    return { text: '', count: 0 };
  }
  const secretRes = redactSecrets(input);
  let out = secretRes.text;
  HTTP_URL_RE.lastIndex = 0;
  out = out.replace(HTTP_URL_RE, '[URL]');
  return { text: out.trim(), count: secretRes.count };
}

export function utf8ByteLength(value: string): number {
  if (typeof TextEncoder !== 'undefined') {
    return new TextEncoder().encode(value).length;
  }
  return Buffer.byteLength(value, 'utf8');
}

/**
 * Applies client-side secret/URL redaction across an AnalyzeStatusPayload, re-clamps every field
 * to the backend's Unicode rune limits so redaction replacements never trigger HTTP 400, and
 * enforces the unified 24 KiB UTF-8 payload budget via deterministic priority trimming.
 */
export function redactAndBoundAnalyzePayload(payload: AnalyzeStatusPayload): {
  payload: AnalyzeStatusPayload;
  redactionCount: number;
  truncated: boolean;
} {
  let redactionCount = 0;
  const scrub = (s: string | undefined, maxRunes: number): string | undefined => {
    if (!s) {
      return s;
    }
    const res = redactSecretsAndURLs(s);
    redactionCount += res.count;
    const clamped = clampRunes(res.text, maxRunes);
    return clamped || undefined;
  };
  const scrubRequired = (s: string, maxRunes: number, fallback: string): string => {
    return scrub(s, maxRunes) ?? fallback;
  };

  const scrubbedScopeVal = payload.scope ? scrub(payload.scope.labelValue, MAX_LABEL_VAL_RUNES) : undefined;
  const out: AnalyzeStatusPayload = {
    ...payload,
    scope:
      payload.scope && scrubbedScopeVal
        ? { labelName: payload.scope.labelName, labelValue: scrubbedScopeVal }
        : undefined,
    userFocus: scrub(payload.userFocus, MAX_ANALYZE_FOCUS_RUNES),
    dashboardContext: payload.dashboardContext
      ? {
          ...payload.dashboardContext,
          title: scrubRequired(payload.dashboardContext.title, MAX_TITLE_RUNES, 'Dashboard'),
        }
      : undefined,
    evidence: {
      collectedAt: payload.evidence.collectedAt,
      alerts: payload.evidence.alerts.map((a) => {
        const cleanLabels: Record<string, string> | undefined = a.labels
          ? Object.fromEntries(
              Object.entries(a.labels).map(([k, v]) => [k, scrub(v, MAX_LABEL_VAL_RUNES) ?? ''])
            )
          : undefined;
        return {
          ...a,
          alertName: scrubRequired(a.alertName, MAX_NAME_RUNES, 'UnnamedAlert'),
          severity: scrub(a.severity, MAX_SEVERITY_RUNES),
          labels: cleanLabels,
          summary: scrub(a.summary, MAX_SUMMARY_RUNES),
          description: scrub(a.description, MAX_DESC_RUNES),
        };
      }),
      rules: payload.evidence.rules.map((r) => ({
        ...r,
        ruleName: scrubRequired(r.ruleName, MAX_NAME_RUNES, 'UnnamedRule'),
        group: scrub(r.group, MAX_GROUP_RUNES),
        lastError: scrub(r.lastError, MAX_ERROR_RUNES),
      })),
      metrics: payload.evidence.metrics.map((m) => ({
        ...m,
        panelTitle: scrubRequired(m.panelTitle, MAX_TITLE_RUNES, 'Panel'),
        unit: scrub(m.unit, MAX_UNIT_RUNES),
        seriesLabel: scrub(m.seriesLabel, MAX_SERIES_LABEL_RUNES),
        errorText: scrub(m.errorText, MAX_ERROR_RUNES),
      })),
      statusNotes: payload.evidence.statusNotes?.map((n) => ({
        ...n,
        message: scrubRequired(n.message, MAX_SUMMARY_RUNES, 'Status checked.'),
      })),
    },
    collectionWarnings: payload.collectionWarnings?.map((w) => ({
      ...w,
      message: scrubRequired(w.message, MAX_SUMMARY_RUNES, 'Collection warning.'),
    })),
  };

  // Reserve space for formatting differences and an optional evidence_truncated warning.
  const targetMaxBytes = MAX_ANALYZE_PAYLOAD_BYTES - 512;
  let truncated = false;

  const fits = (limit: number): boolean => utf8ByteLength(JSON.stringify(out)) <= limit;

  const trimEvidenceOnce = (): boolean => {
    if (
      out.evidence.alerts.length > 3 &&
      out.evidence.alerts.length >= out.evidence.metrics.length &&
      out.evidence.alerts.length >= out.evidence.rules.length
    ) {
      out.evidence.alerts.pop();
      return true;
    }
    if (out.evidence.rules.length > 2 && out.evidence.rules.length >= out.evidence.metrics.length) {
      out.evidence.rules.pop();
      return true;
    }
    if (out.evidence.metrics.length > 2) {
      out.evidence.metrics.pop();
      return true;
    }
    if (out.evidence.alerts.length > 1) {
      out.evidence.alerts.pop();
      return true;
    }
    if (out.evidence.rules.length > 0) {
      out.evidence.rules.pop();
      return true;
    }
    if (out.evidence.metrics.length > 1) {
      out.evidence.metrics.pop();
      return true;
    }
    if (out.collectionWarnings && out.collectionWarnings.length > 2) {
      out.collectionWarnings.pop();
      return true;
    }
    return false;
  };

  while (!fits(targetMaxBytes)) {
    truncated = true;
    if (!trimEvidenceOnce()) {
      break;
    }
  }

  if (truncated) {
    const warnings: CollectionWarningItem[] = out.collectionWarnings ? [...out.collectionWarnings] : [];
    const hasTruncWarn = warnings.some((w) => w.code === 'evidence_truncated');
    if (!hasTruncWarn && warnings.length < 12) {
      warnings.push({
        id: nextWarnId(warnings),
        code: 'evidence_truncated',
        message: 'Evidence bundle was trimmed to stay within the 24 KiB analysis payload limit.',
      });
    }
    out.collectionWarnings = warnings;
    // Re-check strict 24 KiB ceiling after appending the truncation warning.
    while (!fits(MAX_ANALYZE_PAYLOAD_BYTES - 64)) {
      if (!trimEvidenceOnce()) {
        break;
      }
    }
  }

  return { payload: out, redactionCount, truncated };
}
