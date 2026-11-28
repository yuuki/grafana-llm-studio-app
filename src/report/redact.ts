import {
  AnalyzeStatusPayload,
  CollectionWarningItem,
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
    re: /-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----/g,
    replacement: '[REDACTED_KEY]',
  },
  {
    re: /\b(?:https?|postgres(?:ql)?|rediss?|mysql|mongodb(?:\+srv)?|amqps?):\/\/[^:\s/@"'<>]+:[^@\s/"'<>]+@[^\s"'<>]+/gi,
    replacement: '[REDACTED_URI]',
  },
  {
    re: /\b(authorization\s*:\s*(?:bearer|basic)\s+|cookie\s*:\s*)[^\s;,\]"']+/gi,
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
    re: /\b((?:password|passwd|api_?key|access_key|client_secret|access_token|auth_token)\s*=\s*)[^\s,;'"&]+/gi,
    replacement: '$1[REDACTED]',
  },
];

const HTTP_URL_RE = /\bhttps?:\/\/[^\s"'<>()]+/gi;

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
 * Applies client-side secret/URL redaction across an AnalyzeStatusPayload and enforces
 * the unified 24 KiB UTF-8 payload budget via deterministic priority trimming.
 */
export function redactAndBoundAnalyzePayload(payload: AnalyzeStatusPayload): {
  payload: AnalyzeStatusPayload;
  redactionCount: number;
  truncated: boolean;
} {
  let redactionCount = 0;
  const scrub = (s?: string): string | undefined => {
    if (!s) {
      return s;
    }
    const res = redactSecretsAndURLs(s);
    redactionCount += res.count;
    return res.text;
  };

  const out: AnalyzeStatusPayload = {
    ...payload,
    userFocus: scrub(payload.userFocus),
    dashboardContext: payload.dashboardContext
      ? {
          ...payload.dashboardContext,
          title: scrub(payload.dashboardContext.title) ?? payload.dashboardContext.title,
        }
      : undefined,
    evidence: {
      collectedAt: payload.evidence.collectedAt,
      alerts: payload.evidence.alerts.map((a) => {
        const cleanLabels: Record<string, string> | undefined = a.labels
          ? Object.fromEntries(
              Object.entries(a.labels).map(([k, v]) => [k, scrub(v) ?? ''])
            )
          : undefined;
        return {
          ...a,
          alertName: scrub(a.alertName) ?? a.alertName,
          severity: scrub(a.severity),
          labels: cleanLabels,
          summary: scrub(a.summary),
          description: scrub(a.description),
        };
      }),
      rules: payload.evidence.rules.map((r) => ({
        ...r,
        ruleName: scrub(r.ruleName) ?? r.ruleName,
        group: scrub(r.group),
        lastError: scrub(r.lastError),
      })),
      metrics: payload.evidence.metrics.map((m) => ({
        ...m,
        panelTitle: scrub(m.panelTitle) ?? m.panelTitle,
        seriesLabel: scrub(m.seriesLabel),
        errorText: scrub(m.errorText),
      })),
      statusNotes: payload.evidence.statusNotes?.map((n) => ({
        ...n,
        message: scrub(n.message) ?? n.message,
      })),
    },
    collectionWarnings: payload.collectionWarnings?.map((w) => ({
      ...w,
      message: scrub(w.message) ?? w.message,
    })),
  };

  // Reserve a small margin for formatting differences so backend Marshal stays <= 24 KiB.
  const targetMaxBytes = MAX_ANALYZE_PAYLOAD_BYTES - 512;
  let truncated = false;

  const fits = (): boolean => utf8ByteLength(JSON.stringify(out)) <= targetMaxBytes;

  // Deterministic priority trimming: drop lower-priority tail items while preserving top items.
  while (!fits()) {
    truncated = true;
    if (out.evidence.alerts.length > 3 && out.evidence.alerts.length >= out.evidence.metrics.length && out.evidence.alerts.length >= out.evidence.rules.length) {
      out.evidence.alerts.pop();
    } else if (out.evidence.rules.length > 2 && out.evidence.rules.length >= out.evidence.metrics.length) {
      out.evidence.rules.pop();
    } else if (out.evidence.metrics.length > 2) {
      out.evidence.metrics.pop();
    } else if (out.evidence.alerts.length > 1) {
      out.evidence.alerts.pop();
    } else if (out.evidence.rules.length > 0) {
      out.evidence.rules.pop();
    } else if (out.evidence.metrics.length > 1) {
      out.evidence.metrics.pop();
    } else {
      break;
    }
  }

  if (truncated) {
    const warnings: CollectionWarningItem[] = out.collectionWarnings ? [...out.collectionWarnings] : [];
    const hasTruncWarn = warnings.some((w) => w.code === 'evidence_truncated');
    if (!hasTruncWarn && warnings.length < 12) {
      warnings.push({
        id: `ev-warn-${warnings.length + 1}`,
        code: 'evidence_truncated',
        message: 'Evidence bundle was trimmed to stay within the 24 KiB analysis payload limit.',
      });
    }
    out.collectionWarnings = warnings;
  }

  return { payload: out, redactionCount, truncated };
}
