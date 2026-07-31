import { useEffect, useState } from 'react';
import {
  DisconnectOutlined,
  LoadingOutlined,
  RocketOutlined,
  SmileOutlined
} from '@ant-design/icons';
import { Button, Divider, Result, Spin } from 'antd';
import { useTranslation } from 'react-i18next';
import semver from 'semver';

import * as api from '@/api/application.ts';

type UpdateProps = {
  setIsLocked: (isClosable: boolean) => void;
};

// Firmware update, pointed at OUR release channel (the server's
// /api/application/update installs our GitHub-released bundle, never
// cdn.sipeed.com). Reached over the AllMyStuff mesh it needs no device
// password; on the LAN the normal KVM login applies. Sipeed's preview channel
// and offline-upload update are intentionally gone — our channel is the one
// update path.
export const Update = ({ setIsLocked }: UpdateProps) => {
  const { t } = useTranslation();

  const [status, setStatus] = useState('');
  const [currentVersion, setCurrentVersion] = useState('');
  const [latestVersion, setLatestVersion] = useState('');
  const [errMsg, setErrMsg] = useState('');
  // Why the channel lookup failed, straight from the device — so this says
  // which wall it hit rather than leaving everyone to guess.
  const [latestError, setLatestError] = useState('');

  useEffect(() => {
    // Page load is incidental, not a request to go and look — take the cached
    // answer. The button below passes `true` and gets a real check.
    checkForUpdates(false);
  }, []);

  function checkForUpdates(refresh = true) {
    if (status === 'loading') return;
    setStatus('loading');

    api
      .getVersion(refresh)
      .then((rsp: any) => {
        if (rsp.code !== 0 || !rsp.data) {
          setStatus('failed');
          setErrMsg(t('settings.update.queryFailed'));
          return;
        }

        setCurrentVersion(rsp.data.current);

        if (rsp.data?.latest) {
          setLatestVersion(rsp.data.latest);
          const isLatest = semver.gte(rsp.data.current, rsp.data.latest);
          setStatus(isLatest ? 'latest' : 'outdated');
        } else {
          // No `latest` means the server got no answer from the release
          // channel — the device has no route, no DNS, or a clock too far off
          // for TLS. That is neither "behind" nor "up to date", and it used to
          // be reported as the latter: a device that had simply lost its
          // uplink showed "You already have the latest version", which reads
          // as a checked fact. A released build then looks like one that never
          // shipped, and the search starts in the wrong place entirely.
          setLatestError(rsp.data?.latestError ?? '');
          setStatus('unreachable');
        }
      })
      .catch(() => {
        setStatus('failed');
        setErrMsg(t('settings.update.queryFailed'));
      });
  }

  function update() {
    if (status !== 'outdated') return;

    setIsLocked(true);
    setStatus('updating');

    api
      .update()
      .then((rsp: any) => {
        if (rsp.code !== 0) {
          setStatus('failed');
          setErrMsg(t('settings.update.updateFailed'));
        }
      })
      .finally(() => {
        setTimeout(() => {
          setIsLocked(false);
          setErrMsg('');

          window.location.reload();
        }, 12000);
      });
  }

  return (
    <>
      <div className="text-base">{t('settings.update.title')}</div>
      <Divider className="opacity-50" />

      <div className="flex min-h-[320px] flex-col justify-between">
        {status === 'loading' && (
          <div className="flex justify-center pt-24">
            <Spin indicator={<LoadingOutlined spin />} size="large" />
          </div>
        )}

        {status === 'updating' && (
          <div className="flex flex-col items-center justify-center space-y-10 pb-10 pt-24">
            <Spin size="large" />
            <span className="text-neutral-500">{t('settings.update.updating')}</span>
          </div>
        )}

        {status === 'latest' && (
          <Result
            status="success"
            icon={<SmileOutlined />}
            title={currentVersion}
            subTitle={t('settings.update.isLatest')}
            extra={[
              <Button key="confirm" onClick={() => checkForUpdates(true)}>
                {t('settings.update.title')}
              </Button>
            ]}
          />
        )}

        {status === 'outdated' && (
          <Result
            status="warning"
            icon={<RocketOutlined />}
            title={`${currentVersion} -> ${latestVersion}`}
            subTitle={t('settings.update.available')}
            extra={[
              <Button key="confirm" type="primary" onClick={update}>
                {t('settings.update.confirm')}
              </Button>
            ]}
          />
        )}

        {status === 'unreachable' && (
          <Result
            status="warning"
            icon={<DisconnectOutlined />}
            title={currentVersion}
            subTitle={
              latestError
                ? `${t('settings.update.unreachable')} (${latestError})`
                : t('settings.update.unreachable')
            }
            extra={[
              <Button key="confirm" onClick={() => checkForUpdates(true)}>
                {t('settings.update.title')}
              </Button>
            ]}
          />
        )}

        {status === 'failed' && <Result subTitle={errMsg} />}

        <div className="flex justify-center">
          <Button
            type="link"
            size="small"
            href="https://github.com/mrjeeves/NanoKVM/blob/main/CHANGELOG.md"
            target="_blank"
          >
            CHANGELOG
          </Button>
        </div>
      </div>
    </>
  );
};
