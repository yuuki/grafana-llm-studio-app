import { LOCALE_RESOURCES } from '../i18n';
import { ReportLanguage, StatusReportIR } from '../ir/reportTypes';

export interface MarkdownExportMeta {
  generatedAt?: string;
  window: string;
  baselineMode: string;
  dashboardTitle?: string;
  datasourceUid?: string;
  scopeText?: string;
}

/**
 * Escapes Markdown control characters and flattens newlines so untrusted alert/LLM prose
 * cannot inject Markdown links, headings, or broken table/list structures when pasted into
 * Slack, GitHub, or Jira.
 */
export function escapeMarkdownInline(raw: string): string {
  return raw
    .replace(/[\r\n]+/g, ' ')
    .replace(/\\/g, '\\\\')
    .replace(/([`*_[\]()#|<>])/g, '\\$1')
    .trim();
}

function getReportLabels(lang: ReportLanguage) {
  const res = LOCALE_RESOURCES[lang] ?? LOCALE_RESOURCES.en;
  return res.report.markdown;
}

/**
 * Deterministically formats a validated StatusReportIR as Markdown using headings in the
 * exact report language (independent of the browser's Grafana UI locale).
 */
export function formatStatusReportMarkdown(
  report: StatusReportIR,
  language: ReportLanguage,
  meta: MarkdownExportMeta
): string {
  const labels = getReportLabels(language);
  const lines: string[] = [];

  lines.push(`# ${labels.title}`);
  lines.push('');
  lines.push(`- **${labels.status}:** \`${report.overallStatus}\` (${labels.confidence}: \`${report.confidence}\`)`);
  lines.push(`- **${labels.window}:** \`${meta.window}\` (${labels.baseline}: \`${meta.baselineMode}\`)`);
  if (meta.dashboardTitle) {
    lines.push(`- **${labels.dashboard}:** ${escapeMarkdownInline(meta.dashboardTitle)}`);
  }
  if (meta.scopeText) {
    lines.push(`- **${labels.scope}:** \`${escapeMarkdownInline(meta.scopeText)}\``);
  }
  if (meta.generatedAt) {
    lines.push(`- **${labels.generatedAt}:** ${escapeMarkdownInline(meta.generatedAt)}`);
  }
  lines.push('');

  lines.push(`## ${labels.executiveSummary}`);
  lines.push('');
  lines.push(escapeMarkdownInline(report.executiveSummary));
  lines.push('');

  lines.push(`## ${labels.findings}`);
  lines.push('');
  if (report.findings.length === 0) {
    lines.push(`- ${labels.none}`);
  } else {
    for (const f of report.findings) {
      const refs = f.evidenceIds.map((id) => `\`${id}\``).join(', ');
      lines.push(
        `- **[${f.severity.toUpperCase()} / ${f.category}] ${escapeMarkdownInline(f.title)}** (\`${f.id}\`): ${escapeMarkdownInline(f.detail)} (${labels.evidence}: ${refs})`
      );
    }
  }
  lines.push('');

  if (report.timeline.length > 0) {
    lines.push(`## ${labels.timeline}`);
    lines.push('');
    for (const item of report.timeline) {
      const refs = item.evidenceIds.map((id) => `\`${id}\``).join(', ');
      lines.push(`- \`${escapeMarkdownInline(item.timestamp)}\` — ${escapeMarkdownInline(item.event)} (${refs})`);
    }
    lines.push('');
  }

  if (report.hypotheses.length > 0) {
    lines.push(`## ${labels.hypotheses}`);
    lines.push('');
    for (const h of report.hypotheses) {
      const refs = h.evidenceIds.map((id) => `\`${id}\``).join(', ');
      lines.push(`- **[${h.likelihood}] ${escapeMarkdownInline(h.title)}**: ${escapeMarkdownInline(h.rationale)} (${refs})`);
      for (const step of h.verificationSteps) {
        lines.push(`  - ${labels.verify}: ${escapeMarkdownInline(step)}`);
      }
    }
    lines.push('');
  }

  if (report.recommendedActions.length > 0) {
    lines.push(`## ${labels.recommendedActions}`);
    lines.push('');
    for (const act of report.recommendedActions) {
      const rel = act.relatedFindingIds.length > 0 ? ` (${act.relatedFindingIds.map((id) => `\`${id}\``).join(', ')})` : '';
      lines.push(`- [ ] **[${act.priority}]** ${escapeMarkdownInline(act.action)}${rel}`);
    }
    lines.push('');
  }

  if (report.dataGaps.length > 0) {
    lines.push(`## ${labels.dataGaps}`);
    lines.push('');
    for (const gap of report.dataGaps) {
      lines.push(`- ${escapeMarkdownInline(gap)}`);
    }
    lines.push('');
  }

  return lines.join('\n').trim() + '\n';
}

/**
 * Copies text to the clipboard with a fallback for non-HTTPS (air-gapped HTTP) contexts
 * where navigator.clipboard is undefined.
 */
export async function copyTextWithFallback(text: string): Promise<boolean> {
  if (typeof navigator !== 'undefined' && navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Fall through to legacy textarea copy.
    }
  }
  if (typeof document === 'undefined' || !document.body) {
    return false;
  }
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.left = '-9999px';
    document.body.appendChild(ta);
    ta.select();
    const ok = typeof document.execCommand === 'function' ? document.execCommand('copy') : false;
    document.body.removeChild(ta);
    return ok;
  } catch {
    return false;
  }
}
