import React, { Suspense } from 'react';
import { ConfigProvider, Spin, theme } from 'antd';
import ReactDOM from 'react-dom/client';
import { ErrorBoundary } from 'react-error-boundary';
import { HelmetProvider } from 'react-helmet-async';
import { RouterProvider } from 'react-router-dom';

import { MainError } from './components/main-error.tsx';
import { router } from './router';

import '@fontsource-variable/inter';
import './i18n';
import './assets/styles/index.css';

const renderApp = () => {
  // AllMyStuff design language: deep violet-tinted dark, one hot-magenta accent,
  // Inter, softly rounded. Colours + font only — layout/behaviour is untouched.
  // Keeps antd's darkAlgorithm and just re-seeds its tokens (magenta primary,
  // violet-tinted base bg/ink, the AllMyStuff status hues, a rounder radius).
  const themeConfig = {
    algorithm: theme.darkAlgorithm,
    token: {
      colorPrimary: '#f11ea1',
      colorInfo: '#f11ea1',
      colorLink: '#ff77c2',
      colorSuccess: '#5edb81',
      colorWarning: '#efac44',
      colorError: '#fd617a',
      colorBgBase: '#070711',
      colorTextBase: '#e7e7f0',
      borderRadius: 10,
      fontFamily:
        "'Inter Variable', 'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, system-ui, sans-serif"
    },
    components: {
      Collapse: {
        headerPadding: 0,
        contentPadding: 0
      }
    }
  };

  return ReactDOM.createRoot(document.getElementById('root')!).render(
    <React.StrictMode>
      <Suspense
        fallback={
          <div className="flex h-screen w-screen items-center justify-center">
            <Spin size="large" />
          </div>
        }
      >
        <ErrorBoundary FallbackComponent={MainError}>
          <HelmetProvider>
            <ConfigProvider theme={themeConfig}>
              <RouterProvider router={router} />
            </ConfigProvider>
          </HelmetProvider>
        </ErrorBoundary>
      </Suspense>
    </React.StrictMode>
  );
};

if (import.meta.env.MODE === 'mocked') {
  const { worker } = await import('./mocks/browser');
  worker.start().then(() => {
    return renderApp();
  });
}

renderApp();
