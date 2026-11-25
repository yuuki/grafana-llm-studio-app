import grafanaConfig from '@grafana/eslint-config';

export default [
  {
    ignores: ['.github', '**/dist/', '**/coverage/', 'playwright-report/', 'test-results/', 'bench-results/'],
  },
  ...grafanaConfig,
  {
    name: 'yuuki-llmstudio-app/defaults',
    files: ['**/*.{ts,tsx,js,jsx}'],
    rules: {
      'react/prop-types': 'off',
      'react/react-in-jsx-scope': 'off',
      'react-hooks/exhaustive-deps': 'warn',
    },
  },
];
