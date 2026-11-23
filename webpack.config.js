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
    externals: [
      'lodash',
      'react',
      'react-dom',
      'react-router-dom',
      'rxjs',
      '@emotion/css',
      '@grafana/data',
      '@grafana/i18n',
      '@grafana/runtime',
      '@grafana/schema',
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
          { from: 'LICENSE', to: '.', noErrorOnMissing: true },
        ],
      }),
    ],
  };
};
