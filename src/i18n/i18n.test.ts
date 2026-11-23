import React from 'react';
import { render, screen } from '@testing-library/react';
import i18next from 'i18next';
import {
  DEFAULT_LOCALE,
  getActiveLocale,
  initStudioI18n,
  LOCALE_RESOURCES,
  resolveStudioLocale,
  setStudioLocale,
  STUDIO_I18N_NS,
  t,
  useStudioTranslation,
} from './index';

function flattenKeys(obj: Record<string, unknown>, prefix = ''): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(obj)) {
    const fullKey = prefix ? `${prefix}.${key}` : key;
    if (typeof value === 'object' && value !== null && !Array.isArray(value)) {
      Object.assign(out, flattenKeys(value as Record<string, unknown>, fullKey));
    } else if (typeof value === 'string') {
      out[fullKey] = value;
    } else {
      throw new Error(`Unexpected non-string locale value at "${fullKey}"`);
    }
  }
  return out;
}

function extractPlaceholders(template: string): string[] {
  return Array.from(template.matchAll(/\{\{\s*([^}\s]+)\s*\}\}/g), (m) => m[1]).sort();
}

describe('Studio i18n shared i18next integration & locale parity', () => {
  afterEach(async () => {
    await setStudioLocale(DEFAULT_LOCALE);
  });

  test('en and ja locale bundles have identical keys, non-empty strings, and matching placeholders', () => {
    const enFlat = flattenKeys(LOCALE_RESOURCES.en as unknown as Record<string, unknown>);
    const jaFlat = flattenKeys(LOCALE_RESOURCES.ja as unknown as Record<string, unknown>);

    const enKeys = Object.keys(enFlat).sort();
    const jaKeys = Object.keys(jaFlat).sort();
    expect(jaKeys).toEqual(enKeys);

    for (const key of enKeys) {
      expect(enFlat[key].trim().length).toBeGreaterThan(0);
      expect(jaFlat[key].trim().length).toBeGreaterThan(0);
      expect(extractPlaceholders(jaFlat[key])).toEqual(extractPlaceholders(enFlat[key]));
    }
  });

  test('defaults to English (en) and switches cleanly between en, en-US, ja, ja-JP, and unsupported locales', async () => {
    initStudioI18n();
    expect(getActiveLocale()).toBe('en');
    expect(t('studio.actions.generateDraft')).toBe('Generate Dashboard Draft');
    expect(t('appConfig.actions.save')).toBe('Save settings');

    await setStudioLocale('ja');
    expect(getActiveLocale()).toBe('ja');
    expect(t('studio.actions.generateDraft')).toBe('ダッシュボード案を生成');
    expect(t('appConfig.actions.save')).toBe('設定を保存');

    await setStudioLocale('ja-JP');
    expect(getActiveLocale()).toBe('ja');
    expect(t('studio.validation.category.ok')).toBe('正常');

    await setStudioLocale('en-US');
    expect(getActiveLocale()).toBe('en');
    expect(t('studio.validation.category.ok')).toBe('OK');

    // Unsupported Grafana locales must fall back to English rather than leaking raw translation keys.
    await setStudioLocale('fr-FR');
    expect(getActiveLocale()).toBe('en');
    expect(t('studio.actions.generateDraft')).toBe('Generate Dashboard Draft');
  });

  test('resolveStudioLocale normalizes BCP-47 locale tags safely', () => {
    expect(resolveStudioLocale(undefined)).toBe('en');
    expect(resolveStudioLocale('')).toBe('en');
    expect(resolveStudioLocale('en')).toBe('en');
    expect(resolveStudioLocale('en-US')).toBe('en');
    expect(resolveStudioLocale('ja')).toBe('ja');
    expect(resolveStudioLocale('ja-JP')).toBe('ja');
    expect(resolveStudioLocale('JA_jp')).toBe('ja');
    expect(resolveStudioLocale('de-DE')).toBe('en');
  });

  test('registers plugin namespace onto a pre-initialized Grafana i18next instance without clobbering core state', async () => {
    const grafanaInstance = i18next.createInstance();
    await grafanaInstance.init({
      lng: 'ja-JP',
      fallbackLng: 'en-US',
      ns: ['grafana'],
      defaultNS: 'grafana',
      resources: {
        'ja-JP': { grafana: { greeting: 'こんにちは Grafana' } },
        'en-US': { grafana: { greeting: 'Hello Grafana' } },
      },
      initAsync: false,
      showSupportNotice: false,
    });

    initStudioI18n(grafanaInstance);

    expect(grafanaInstance.language).toBe('ja-JP');
    expect(grafanaInstance.t('greeting', { ns: 'grafana' })).toBe('こんにちは Grafana');
    expect(grafanaInstance.t('studio.actions.generateDraft', { ns: STUDIO_I18N_NS })).toBe(
      'ダッシュボード案を生成'
    );
    expect(grafanaInstance.t('studio.actions.generateDraft', { ns: STUDIO_I18N_NS, lng: 'en-US' })).toBe(
      'Generate Dashboard Draft'
    );
  });

  test('does not HTML-escape interpolated error strings containing quotes or slashes', async () => {
    await setStudioLocale('en');
    const rawErr = 'parse error: unexpected ")" in http_requests_total{service="checkout-api"} /api/ds/query';
    expect(t('studio.errors.validationFailed', { error: rawErr })).toBe(
      `Validation & preview failed: ${rawErr}`
    );
  });

  test('works cleanly with Grafana SystemJS Module Namespace Objects that omit EventEmitter prototype methods (.on/.off)', async () => {
    const realInstance = i18next.createInstance();
    await realInstance.init({
      lng: 'ja-JP',
      fallbackLng: 'en-US',
      resources: {},
      initAsync: false,
      showSupportNotice: false,
    });

    // Simulate SystemJS `_export(getI18nInstance())`: only own enumerable properties are exposed.
    const systemJsNamespace = Object.create(null) as Record<string, unknown>;
    for (const key of Object.keys(realInstance)) {
      systemJsNamespace[key] = (realInstance as unknown as Record<string, unknown>)[key];
    }
    expect(typeof systemJsNamespace.on).toBe('undefined');
    expect(typeof systemJsNamespace.off).toBe('undefined');

    const Probe: React.FC = () => {
      const { locale, i18n } = useStudioTranslation(
        systemJsNamespace as unknown as Parameters<typeof useStudioTranslation>[0]
      );
      return React.createElement(
        'div',
        { 'data-testid': 'probe' },
        `${locale}:${String(i18n.t('studio.actions.generateDraft', { ns: STUDIO_I18N_NS, lng: locale }))}`
      );
    };

    render(React.createElement(Probe));
    expect(screen.getByTestId('probe').textContent).toBe('ja:ダッシュボード案を生成');
  });
});
