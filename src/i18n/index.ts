import i18next, { type i18n as I18nInstance } from 'i18next';
import { useEffect, useState } from 'react';
import en from './locales/en.json';
import ja from './locales/ja.json';

export const STUDIO_I18N_NS = 'yuuki-llmstudio-app';
export const DEFAULT_LOCALE = 'en';
export const SUPPORTED_LOCALES = ['en', 'ja'] as const;

export type SupportedLocale = (typeof SUPPORTED_LOCALES)[number];

type NestedKeyOf<T> = T extends Record<string, unknown>
  ? {
      [K in keyof T & string]: T[K] extends Record<string, unknown>
        ? `${K}.${NestedKeyOf<T[K]>}`
        : K;
    }[keyof T & string]
  : never;

export type TranslationKey = NestedKeyOf<typeof en>;

export const LOCALE_RESOURCES: Record<SupportedLocale, typeof en> = {
  en,
  ja,
};

const REGISTERED_LANGUAGE_MAP: ReadonlyArray<readonly [string, typeof en]> = [
  ['en', en],
  ['en-US', en],
  ['ja', ja],
  ['ja-JP', ja],
];

/**
 * Safely unwraps Grafana's shared i18next runtime (AMD/CJS interop may wrap it under `.default`).
 */
export function getSharedI18n(): I18nInstance {
  const candidate = i18next as I18nInstance & { default?: I18nInstance };
  return candidate?.default ?? candidate;
}

/**
 * Normalizes any BCP-47 locale tag from Grafana's shared i18next runtime (e.g. `ja-JP`, `en-US`, `fr-FR`)
 * into a bundled plugin locale (`ja` or default `en`).
 */
export function resolveStudioLocale(language?: string | null): SupportedLocale {
  if (!language) {
    return DEFAULT_LOCALE;
  }
  const base = language.trim().toLowerCase().split(/[-_]/)[0];
  return base === 'ja' ? 'ja' : DEFAULT_LOCALE;
}

function registerBundles(instance: I18nInstance): void {
  if (typeof instance.addResourceBundle !== 'function') {
    return;
  }
  for (const [lng, bundle] of REGISTERED_LANGUAGE_MAP) {
    const alreadyLoaded =
      typeof instance.hasResourceBundle === 'function' &&
      instance.hasResourceBundle(lng, STUDIO_I18N_NS);
    if (!alreadyLoaded) {
      instance.addResourceBundle(lng, STUDIO_I18N_NS, bundle, true, false);
    }
  }
}

/**
 * Registers the plugin's bundled locale resources onto Grafana's shared i18next instance.
 * When Grafana has already initialized i18next, we never re-initialize or mutate Grafana's core settings.
 * In standalone / Jest environments where i18next is not yet initialized, we initialize synchronously with `en` default.
 */
export function initStudioI18n(instance: I18nInstance = getSharedI18n()): I18nInstance {
  if (!instance.isInitialized && typeof instance.init === 'function') {
    void instance.init({
      lng: DEFAULT_LOCALE,
      fallbackLng: DEFAULT_LOCALE,
      ns: [STUDIO_I18N_NS],
      defaultNS: STUDIO_I18N_NS,
      resources: {
        en: { [STUDIO_I18N_NS]: en },
        'en-US': { [STUDIO_I18N_NS]: en },
        ja: { [STUDIO_I18N_NS]: ja },
        'ja-JP': { [STUDIO_I18N_NS]: ja },
      },
      interpolation: {
        escapeValue: false,
      },
      returnEmptyString: false,
      initAsync: false,
      showSupportNotice: false,
    });
  }
  registerBundles(instance);
  return instance;
}

export function getActiveLocale(instance: I18nInstance = initStudioI18n()): SupportedLocale {
  return resolveStudioLocale(instance.resolvedLanguage || instance.language);
}

/**
 * Changes the active language on the shared i18next instance (primarily useful for tests and locale switching).
 */
export async function setStudioLocale(
  language: string,
  instance: I18nInstance = initStudioI18n()
): Promise<SupportedLocale> {
  await instance.changeLanguage(language);
  return getActiveLocale(instance);
}

/**
 * Translates a strongly-typed plugin key using Grafana's shared i18next runtime.
 * Always disables HTML escaping so interpolated PromQL snippets, URLs, and API errors render verbatim in React.
 */
export function t(key: TranslationKey, values?: Record<string, string | number>): string {
  const instance = initStudioI18n();
  const lng = getActiveLocale(instance);
  return String(
    instance.t(key, {
      ns: STUDIO_I18N_NS,
      lng,
      fallbackLng: DEFAULT_LOCALE,
      interpolation: { escapeValue: false },
      ...values,
    })
  );
}

/**
 * Lightweight React hook subscribing to shared i18next `languageChanged` events without bundling react-i18next.
 * Note: Grafana's SystemJS loader exposes `i18next` via `_export(getI18nInstance())`, which only copies own
 * enumerable properties and omits `EventEmitter.prototype.on` / `.off`. Guard subscription calls accordingly.
 */
export function useStudioTranslation(instanceOverride?: I18nInstance) {
  const instance = initStudioI18n(instanceOverride);
  const [locale, setLocale] = useState<SupportedLocale>(() => getActiveLocale(instance));

  useEffect(() => {
    const handleLanguageChanged = () => {
      setLocale(getActiveLocale(instance));
    };
    handleLanguageChanged();
    const canSubscribe =
      typeof instance.on === 'function' && typeof instance.off === 'function';
    if (canSubscribe) {
      instance.on('languageChanged', handleLanguageChanged);
    }
    return () => {
      if (canSubscribe) {
        instance.off('languageChanged', handleLanguageChanged);
      }
    };
  }, [instance]);

  return {
    t,
    locale,
    i18n: instance,
  };
}
