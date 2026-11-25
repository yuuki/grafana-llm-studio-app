const path = require('path');
const CopyWebpackPlugin = require('copy-webpack-plugin');

module.exports = (env, argv) => {
  const isProd = argv.mode === 'production';

  return {
    mode: isProd ? 'production' : 'development',
    devtool: isProd ? 'source-map' : 'eval-source-map',
    entry: {
      module: path.resolve(__dirname, 'src/module.ts'),
    },
    output: {
      path: path.resolve(__dirname, 'dist'),
      filename: '[name].js',
      library: {
        type: 'amd',
      },
      clean: false,
    },
    // Strictly aligned with Grafana OSS v12.4.15 (React 18) and v13.3.1 (React 19) sharedDependenciesMap:
    // - Do NOT externalize @grafana/schema or @grafana/i18n (not exposed in SystemJS sharedDependenciesMap)
    // - Do NOT externalize react-router-dom (Grafana exposes v5 under react-router-dom, v6 under react-router)
    // - Do NOT externalize react-dom/client or react-dom/server (only added in Grafana 13, absent in 12.4.15)
    // - DO externalize react/jsx-runtime and react/jsx-dev-runtime so Grafana 13's React 19 runtime works cleanly
    // - DO externalize @emotion/react and i18next to prevent duplicate runtime instances
    externals: [
      'lodash',
      'react',
      'react-dom',
      'react/jsx-runtime',
      'react/jsx-dev-runtime',
      'react-router',
      'rxjs',
      'i18next',
      '@emotion/css',
      '@emotion/react',
      '@grafana/data',
      '@grafana/runtime',
      '@grafana/ui',
    ],
    resolve: {
      extensions: ['.ts', '.tsx', '.js', '.jsx', '.json'],
    },
    module: {
      rules: [
        {
          test: /\.tsx?$/,
          use: [
            {
              loader: 'ts-loader',
              options: {
                transpileOnly: false,
                compilerOptions: {
                  noEmit: false,
                },
              },
            },
          ],
          exclude: /node_modules/,
        },
      ],
    },
    plugins: [
      new CopyWebpackPlugin({
        patterns: [
          { from: 'src/plugin.json', to: '.' },
          { from: 'src/img', to: 'img' },
          { from: 'README.md', to: '.', noErrorOnMissing: true },
          { from: 'CHANGELOG.md', to: '.', noErrorOnMissing: true },
          { from: 'LICENSE', to: '.', noErrorOnMissing: true },
        ],
      }),
    ],
  };
};
