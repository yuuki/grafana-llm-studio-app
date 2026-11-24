import { of, throwError } from 'rxjs';
import { extractApiError } from '../api/studioClient';
import { fetchScopedMetricCatalog, fetchScopeLabelValues } from './metadata';

const mockFetch = jest.fn();

jest.mock('@grafana/runtime', () => ({
  getBackendSrv: () => ({
    fetch: mockFetch,
  }),
}));

describe('Scoped metadata discovery & VictoriaMetrics Resource API routing', () => {
  beforeEach(() => {
    mockFetch.mockReset();
  });

  test('uses Grafana Resource API first for victoriametrics-metrics-datasource', async () => {
    mockFetch.mockReturnValueOnce(
      of({
        data: { status: 'success', data: ['checkout-api', 'orders-api'] },
      })
    );

    const vals = await fetchScopeLabelValues(
      'vm-ds',
      'service',
      3600,
      100,
      undefined,
      'victoriametrics-metrics-datasource'
    );
    expect(vals).toEqual(['checkout-api', 'orders-api']);
    expect(mockFetch).toHaveBeenCalledTimes(1);
    expect(mockFetch.mock.calls[0][0].url).toBe(
      '/api/datasources/uid/vm-ds/resources/api/v1/label/service/values'
    );
  });

  test('falls back from Resource API to proxy endpoint on HTTP 404 for victoriametrics-metrics-datasource', async () => {
    mockFetch
      .mockReturnValueOnce(throwError(() => ({ status: 404, statusText: 'Not Found' })))
      .mockReturnValueOnce(
        of({
          data: { status: 'success', data: ['checkout-api'] },
        })
      );

    const vals = await fetchScopeLabelValues(
      'vm-ds',
      'service',
      3600,
      100,
      undefined,
      'victoriametrics-metrics-datasource'
    );
    expect(vals).toEqual(['checkout-api']);
    expect(mockFetch).toHaveBeenCalledTimes(2);
    expect(mockFetch.mock.calls[0][0].url).toBe(
      '/api/datasources/uid/vm-ds/resources/api/v1/label/service/values'
    );
    expect(mockFetch.mock.calls[1][0].url).toBe(
      '/api/datasources/proxy/uid/vm-ds/api/v1/label/service/values'
    );
  });

  test('uses proxy endpoint directly for standard prometheus datasource', async () => {
    mockFetch
      .mockReturnValueOnce(of({ data: { status: 'success', data: ['http_requests_total'] } }))
      .mockReturnValueOnce(
        of({
          data: {
            status: 'success',
            data: [{ __name__: 'http_requests_total', service: 'checkout-api', status: '200' }],
          },
        })
      )
      .mockReturnValueOnce(of({ data: { status: 'success', data: {} } }));

    const res = await fetchScopedMetricCatalog({
      datasourceUid: 'prom-main',
      datasourceType: 'prometheus',
      scope: { labelName: 'service', labelValue: 'checkout-api' },
    });

    expect(res.items).toHaveLength(1);
    expect(mockFetch.mock.calls[0][0].url).toBe(
      '/api/datasources/proxy/uid/prom-main/api/v1/label/__name__/values'
    );
  });

  test('extractApiError surfaces specific data.message over generic HTTP status text in data.error', () => {
    const err = {
      status: 400,
      data: {
        error: 'Bad Request',
        message: 'ERROR: cannot parse query: unexpected token',
      },
    };
    expect(extractApiError(err)).toBe('HTTP 400: ERROR: cannot parse query: unexpected token');
  });
});
