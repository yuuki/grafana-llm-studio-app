import { AppPlugin } from '@grafana/data';
import { AppConfig, StudioJsonData } from './components/AppConfig/AppConfig';
import { initStudioI18n } from './i18n';
import { StudioPage } from './pages/StudioPage';

initStudioI18n();

export const plugin = new AppPlugin<StudioJsonData>()
  .setRootPage(StudioPage)
  .addConfigPage({
    title: 'Configuration',
    icon: 'cog',
    body: AppConfig,
    id: 'configuration',
  });
