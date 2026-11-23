module.exports = {
  preset: 'ts-jest',
  testEnvironment: 'jsdom',
  setupFiles: ['<rootDir>/jest.setup.js'],
  roots: ['<rootDir>/src'],
  testMatch: ['**/*.test.ts', '**/*.test.tsx'],
  moduleFileExtensions: ['ts', 'tsx', 'js', 'jsx', 'json'],
  moduleNameMapper: {
    '^@grafana/grafana-foundation-sdk/(.*)$':
      '<rootDir>/node_modules/@grafana/grafana-foundation-sdk/dist/$1/index.js',
    '^marked$': '<rootDir>/node_modules/marked/lib/marked.umd.js',
    '^(d3-[a-z-]+)$': '<rootDir>/node_modules/$1/dist/$1.min.js',
  },
  transform: {
    '^.+\\.[tj]sx?$': [
      'ts-jest',
      {
        diagnostics: false,
        tsconfig: {
          allowJs: true,
          module: 'CommonJS',
          moduleResolution: 'node',
        },
      },
    ],
  },
  transformIgnorePatterns: [
    'node_modules/(?!(d3-.*|internmap|delaunator|robust-predicates|ol)/)',
  ],
};
