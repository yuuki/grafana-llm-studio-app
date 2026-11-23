import { MetricCatalogItem } from '../ir/types';

export interface CandidateSelectOptions {
  maxItems?: number;
  maxTotalChars?: number;
}

const DEFAULT_MAX_ITEMS = 40;
const DEFAULT_MAX_CHARS = 11000;

/**
 * Lightweight BM25 + lexical tokenizer for narrowing scoped Prometheus/VictoriaMetrics
 * catalogs to 30-50 most relevant metrics within a strict character/token budget.
 * Runs purely in browser memory for the current user session; never shared across users.
 */
export function selectMetricCandidates(
  catalog: MetricCatalogItem[],
  userQuery: string,
  options: CandidateSelectOptions = {}
): MetricCatalogItem[] {
  const maxItems = Math.min(options.maxItems ?? DEFAULT_MAX_ITEMS, 50);
  const maxTotalChars = options.maxTotalChars ?? DEFAULT_MAX_CHARS;

  if (catalog.length === 0) {
    return [];
  }

  const queryTokens = expandIntentTokens(tokenize(userQuery));
  if (queryTokens.length === 0) {
    return trimToBudget(catalog, maxItems, maxTotalChars);
  }

  const docs = catalog.map((item) => {
    const tokens = [
      ...tokenize(item.name),
      ...tokenize(item.name), // boost metric name weight
      ...tokenize(item.help ?? ''),
      ...tokenize((item.labels ?? []).join(' ')),
      ...tokenize(item.type ?? ''),
    ];
    return { item, tokens };
  });

  const N = docs.length;
  const avgDocLen =
    docs.reduce((acc, d) => acc + d.tokens.length, 0) / Math.max(N, 1);

  const df = new Map<string, number>();
  for (const d of docs) {
    const uniq = new Set(d.tokens);
    for (const tok of uniq) {
      df.set(tok, (df.get(tok) ?? 0) + 1);
    }
  }

  const k1 = 1.5;
  const b = 0.75;

  const scored = docs.map((d) => {
    const tf = new Map<string, number>();
    for (const tok of d.tokens) {
      tf.set(tok, (tf.get(tok) ?? 0) + 1);
    }

    let score = 0;
    for (const qt of queryTokens) {
      const freq = tf.get(qt) ?? 0;
      if (freq === 0) {
        // Also allow prefix lexical match (e.g. "req" -> "requests").
        for (const [docTok, docFreq] of tf.entries()) {
          if (qt.length >= 3 && docTok.startsWith(qt)) {
            const idf = Math.log(1 + (N - (df.get(docTok) ?? 0) + 0.5) / ((df.get(docTok) ?? 0) + 0.5));
            score += 0.5 * idf * ((docFreq * (k1 + 1)) / (docFreq + k1 * (1 - b + b * (d.tokens.length / Math.max(avgDocLen, 1)))));
            break;
          }
        }
        continue;
      }
      const nqi = df.get(qt) ?? 0;
      const idf = Math.log(1 + (N - nqi + 0.5) / (nqi + 0.5));
      score += idf * ((freq * (k1 + 1)) / (freq + k1 * (1 - b + b * (d.tokens.length / Math.max(avgDocLen, 1)))));
    }

    return { item: d.item, score };
  });

  scored.sort((a, b) => {
    if (b.score !== a.score) {
      return b.score - a.score;
    }
    return a.item.name.localeCompare(b.item.name);
  });

  return trimToBudget(
    scored.map((s) => s.item),
    maxItems,
    maxTotalChars
  );
}

function trimToBudget(
  items: MetricCatalogItem[],
  maxItems: number,
  maxTotalChars: number
): MetricCatalogItem[] {
  const out: MetricCatalogItem[] = [];
  let chars = 0;
  for (const item of items) {
    if (out.length >= maxItems) {
      break;
    }
    const serializedLen = JSON.stringify(item).length;
    if (chars + serializedLen > maxTotalChars) {
      break;
    }
    chars += serializedLen;
    out.push(item);
  }
  return out;
}

export function tokenize(input: string): string[] {
  return input
    .toLowerCase()
    .split(/[^a-z0-9\u3040-\u30ff\u4e00-\u9faf]+/)
    .map((t) => t.trim())
    .filter((t) => t.length >= 2);
}

const JA_SYNONYMS: Record<string, string[]> = {
  リクエスト: ['http', 'request', 'requests', 'total', 'rps'],
  エラー: ['error', 'errors', 'status', '5xx', 'requests'],
  レイテンシ: ['latency', 'duration', 'seconds', 'bucket', 'histogram'],
  応答時間: ['latency', 'duration', 'seconds', 'bucket'],
  メモリ: ['memory', 'bytes', 'resident', 'working'],
  使用率: ['usage', 'ratio', 'percent', 'cpu', 'memory'],
  スループット: ['requests', 'rate', 'total', 'ops'],
  red: ['http', 'requests', 'status', 'duration', 'bucket'],
};

function expandIntentTokens(tokens: string[]): string[] {
  const expanded = new Set<string>(tokens);
  for (const t of tokens) {
    for (const [jaKey, syns] of Object.entries(JA_SYNONYMS)) {
      if (t.includes(jaKey)) {
        for (const s of syns) {
          expanded.add(s);
        }
      }
    }
  }
  return Array.from(expanded);
}
