import { useEffect, useState } from 'react';
import { Button, Divider, Tag, Typography } from 'antd';
import { LoaderCircleIcon } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import { getHelpStatus, getMeshStatus, rotateClaimCode, toggleHand } from '@/api/mesh.ts';

type MeshMembership = {
  networkId: string;
  fleet: boolean;
  joining: boolean;
};

type HelpStatus = {
  enabled: boolean;
  asking: boolean;
  supportId: string;
};

type MeshStatus = {
  enabled: boolean;
  connected: boolean;
  nodeId: string;
  label: string;
  joiningMesh: string;
  claimable: boolean;
  owner: string;
  fleetName: string;
  attachedTo: string;
  attachedLabel: string;
  meshes: MeshMembership[];
  publicClaims: boolean;
  claimCode?: string;
};

export const Mesh = () => {
  const { t } = useTranslation();

  const [status, setStatus] = useState<MeshStatus>();
  const [help, setHelp] = useState<HelpStatus>();
  const [errMsg, setErrMsg] = useState('');
  const [rotating, setRotating] = useState(false);
  const [toggling, setToggling] = useState(false);

  function toggleHelp() {
    if (toggling) return;
    setToggling(true);
    toggleHand()
      .then((rsp) => {
        if (rsp.code !== 0) {
          setErrMsg(rsp.msg);
          return;
        }
        setErrMsg('');
        setHelp(rsp.data);
      })
      .catch((err) => {
        setErrMsg(err?.message || t('settings.mesh.queryFailed'));
      })
      .finally(() => setToggling(false));
  }

  function rotateCode() {
    if (rotating) return;
    setRotating(true);
    rotateClaimCode()
      .then((rsp) => {
        if (rsp.code !== 0) {
          setErrMsg(rsp.msg);
          return;
        }
        setErrMsg('');
        setStatus(rsp.data);
      })
      .catch((err) => {
        setErrMsg(err?.message || t('settings.mesh.queryFailed'));
      })
      .finally(() => setRotating(false));
  }

  useEffect(() => {
    function getStatus() {
      getMeshStatus()
        .then((rsp) => {
          if (rsp.code !== 0) {
            setErrMsg(rsp.msg);
            return;
          }

          setErrMsg('');
          setStatus(rsp.data);
        })
        .catch((err) => {
          setErrMsg(err?.message || t('settings.mesh.queryFailed'));
        });
    }

    function getHelp() {
      getHelpStatus()
        .then((rsp) => {
          if (rsp.code === 0) setHelp(rsp.data);
        })
        .catch(() => {
          /* non-fatal: the hand-raise section just stays hidden */
        });
    }

    getStatus();
    getHelp();

    const interval = setInterval(() => {
      getStatus();
      getHelp();
    }, 5000);
    return () => clearInterval(interval);
  }, [t]);

  const attached = status?.attachedLabel || status?.attachedTo;

  return (
    <>
      <div className="text-base">{t('settings.mesh.title')}</div>
      <Divider className="opacity-50" />

      {!status ? (
        <div className="flex w-full items-center justify-center space-x-2 pt-5 text-neutral-500">
          <LoaderCircleIcon className="animate-spin" size={18} />
          <span>{t('settings.mesh.loading')}</span>
        </div>
      ) : !status.enabled ? (
        <div className="pt-5 text-neutral-400">{t('settings.mesh.disabled')}</div>
      ) : (
        <>
          {/* joining mesh */}
          <div className="text-neutral-400">{t('settings.mesh.joiningMesh')}</div>
          <div className="mt-5 flex w-full flex-col items-center space-y-3 rounded-lg bg-neutral-800/50 px-5 py-6">
            {status.joiningMesh ? (
              <>
                <Typography.Text className="break-all text-center font-mono text-2xl" copyable>
                  {status.joiningMesh}
                </Typography.Text>
                <span className="text-center text-sm text-neutral-400">
                  {t('settings.mesh.joiningMeshDesc')}
                </span>
              </>
            ) : (
              <div className="flex items-center space-x-2 text-neutral-500">
                <LoaderCircleIcon className="animate-spin" size={16} />
                <span>{t('settings.mesh.waiting')}</span>
              </div>
            )}
          </div>
          <Divider className="opacity-50" />

          {/* CEC hand raise (Ask for help) */}
          {help?.enabled && (
            <>
              <div className="text-neutral-400">{t('settings.mesh.handRaise')}</div>
              <div className="mt-5 flex w-full flex-col items-center space-y-3 rounded-lg bg-neutral-800/50 px-5 py-6">
                {help.supportId && (
                  <div className="flex w-full items-center justify-between">
                    <span className="text-neutral-400">{t('settings.mesh.supportNumber')}</span>
                    <Typography.Text className="font-mono text-lg" copyable>
                      {help.supportId}
                    </Typography.Text>
                  </div>
                )}
                <span
                  className={`text-center text-sm ${help.asking ? 'text-green-500' : 'text-neutral-400'}`}
                >
                  {help.asking ? t('settings.mesh.handRaised') : t('settings.mesh.handDown')}
                </span>
                <Button
                  type={help.asking ? 'default' : 'primary'}
                  danger={help.asking}
                  loading={toggling}
                  onClick={toggleHelp}
                >
                  {help.asking ? t('settings.mesh.lowerHand') : t('settings.mesh.raiseHand')}
                </Button>
                <span className="text-center text-xs text-neutral-500">
                  {t('settings.mesh.handRaiseDesc')}
                </span>
              </div>
              <Divider className="opacity-50" />
            </>
          )}

          {/* remote claiming (claim code) — shown only while the device is
              claimable with publicClaims enabled in server.yaml. The policy
              itself is deliberately not settable here: config file only. */}
          {status.claimable && (
            <>
              <div className="text-neutral-400">{t('settings.mesh.remoteClaiming')}</div>
              <div className="mt-5 flex w-full flex-col items-center space-y-3 rounded-lg bg-neutral-800/50 px-5 py-6">
                {status.publicClaims && status.claimCode ? (
                  <>
                    <Typography.Text className="break-all text-center font-mono text-xl" copyable>
                      {status.claimCode}
                    </Typography.Text>
                    <span className="text-center text-sm text-neutral-400">
                      {t('settings.mesh.claimCodeDesc')}
                    </span>
                    <Button size="small" loading={rotating} onClick={rotateCode}>
                      {t('settings.mesh.rotateCode')}
                    </Button>
                  </>
                ) : (
                  <span className="text-center text-sm text-neutral-400">
                    {t('settings.mesh.remoteClaimingOff')}
                  </span>
                )}
              </div>
              <Divider className="opacity-50" />
            </>
          )}

          {/* status */}
          <div className="text-neutral-400">{t('settings.mesh.status')}</div>
          <div className="mt-5 flex w-full flex-col space-y-5">
            <div className="flex w-full items-center justify-between">
              <span>{t('settings.mesh.claimState')}</span>
              <span>
                {status.claimable
                  ? t('settings.mesh.claimable')
                  : status.fleetName
                    ? t('settings.mesh.claimedFleet', { name: status.fleetName })
                    : t('settings.mesh.claimed')}
              </span>
            </div>

            <div className="flex w-full items-center justify-between">
              <span>{t('settings.mesh.label')}</span>
              <span>{status.label || '-'}</span>
            </div>

            <div className="flex w-full items-center justify-between">
              <span>{t('settings.mesh.attachedTo')}</span>
              {attached ? (
                <span>{attached}</span>
              ) : (
                <span className="text-neutral-500">{t('settings.mesh.notAttached')}</span>
              )}
            </div>

            <div className="flex w-full items-center justify-between">
              <span>{t('settings.mesh.nodeId')}</span>
              {status.nodeId ? (
                <Typography.Text className="break-all font-mono text-xs" copyable>
                  {status.nodeId}
                </Typography.Text>
              ) : (
                <span>-</span>
              )}
            </div>

            <div className="flex w-full items-center justify-between">
              <span>{t('settings.mesh.connection')}</span>
              <span className={status.connected ? 'text-green-500' : 'text-neutral-500'}>
                {status.connected ? t('settings.mesh.connected') : t('settings.mesh.disconnected')}
              </span>
            </div>
          </div>
          <Divider className="opacity-50" />

          {/* memberships */}
          <div className="text-neutral-400">{t('settings.mesh.memberships')}</div>
          <div className="mt-5 flex w-full flex-col space-y-3">
            {status.meshes.length > 0 ? (
              status.meshes.map((mesh) => (
                <div key={mesh.networkId} className="flex w-full items-center justify-between">
                  <span className="break-all font-mono text-sm">{mesh.networkId}</span>
                  <div className="flex shrink-0 items-center pl-2">
                    {mesh.fleet && <Tag color="blue">{t('settings.mesh.fleet')}</Tag>}
                    {mesh.joining && <Tag color="green">{t('settings.mesh.joining')}</Tag>}
                  </div>
                </div>
              ))
            ) : (
              <span className="text-neutral-500">{t('settings.mesh.noMemberships')}</span>
            )}
          </div>
        </>
      )}

      {errMsg && <div className="pt-5 text-red-500">{errMsg}</div>}
    </>
  );
};
