import React from 'react';
import { Route, Routes } from 'react-router';
import { StatusReportPage } from './StatusReportPage';
import { StudioPage } from './StudioPage';

/**
 * Root multi-page router mounted by AppPlugin.setRootPage(AppRoot).
 * Uses ONLY the shared v6/v7 API subset of externalized `react-router` (never `react-router-dom`)
 * so the bundle stays compatible with both Grafana OSS 12.4.15 (React 18) and 13.3.1 (React 19).
 */
export const AppRoot: React.FC = () => {
  return (
    <Routes>
      <Route path="reports" element={<StatusReportPage />} />
      <Route path="reports/*" element={<StatusReportPage />} />
      <Route path="*" element={<StudioPage />} />
    </Routes>
  );
};
