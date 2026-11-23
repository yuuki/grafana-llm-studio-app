import { AppPlugin } from '@grafana/data';
import { AppConfig, StudioJsonData } from './components/AppConfig/AppConfig';
import { StudioPage } from './pages/StudioPage';

export const plugin = new AppPlugin<StudioJsonData>()
  .setRootPage(StudioPage)
  .addConfigPage({
    title: 'Configuration',
    icon: 'cog',
    body: AppConfig,
    id: 'configuration',
  });
